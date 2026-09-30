#!/usr/bin/env bash
# File: deploy/scripts/check-pins.sh
# Purpose: supply-chain guard (deploy-design §5.1): every external image referenced by the
#   deploy package is pinned by digest and nothing uses `latest`.
#   Dockerfiles: every `ARG *_IMAGE=` default carries @sha256:<64 hex>; every FROM uses such an
#   ARG or a previous stage name. Compose files: every `image:` is either digest-pinned or one
#   of our own locally built images (${LC_IMAGE_PREFIX}-{go,admin,storefront,caddy}:${IMAGE_TAG}).
# Runs as/in: developer/build host, CI; no docker needed. Reads env / secrets: none.
# Used by: smoke.sh S02; reviewers before a digest bump.
# Depends on: grep/sed/awk only.
# Status: DESIGN; verified by smoke S02.
# Change rules: a digest bump = re-resolve with `docker buildx imagetools inspect <tag>`,
#   update the pin + resolve date comment + deploy/README.md ledger, run smoke full.
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

fail=0
bad() {
  printf 'pin FAIL %s: %s\n' "$1" "$2"
  fail=1
}
digest='@sha256:[0-9a-f]{64}'

for df in "$LC_DEPLOY_DIR"/docker/*.Dockerfile; do
  rel=${df#"$LC_REPO_ROOT"/}
  stages=() args=()
  while IFS= read -r line; do
    if [[ "$line" =~ ^ARG[[:space:]]+([A-Z_]+_IMAGE)=(.*)$ ]]; then
      args+=("${BASH_REMATCH[1]}")
      [[ "${BASH_REMATCH[2]}" =~ $digest ]] || bad "$rel" "ARG ${BASH_REMATCH[1]} is not digest-pinned"
    elif [[ "$line" =~ ^FROM[[:space:]]+([^[:space:]]+)([[:space:]]+[Aa][Ss][[:space:]]+([^[:space:]]+))? ]]; then
      ref=${BASH_REMATCH[1]}
      [[ -n "${BASH_REMATCH[3]}" ]] && stages+=("${BASH_REMATCH[3]}")
      if [[ "$ref" =~ ^\$\{([A-Z_]+)\}$ ]]; then
        [[ " ${args[*]} " == *" ${BASH_REMATCH[1]} "* ]] || bad "$rel" "FROM uses undeclared ARG ${BASH_REMATCH[1]}"
      elif [[ " ${stages[*]} " == *" $ref "* ]]; then
        :
      elif [[ ! "$ref" =~ $digest ]]; then
        bad "$rel" "FROM $ref is not digest-pinned"
      fi
    fi
    [[ "$line" =~ :latest([[:space:]@]|$) ]] && bad "$rel" "uses :latest"
  done <"$df"
done

for cf in "$LC_DEPLOY_DIR"/compose*.yml; do
  rel=${cf#"$LC_REPO_ROOT"/}
  while IFS= read -r line; do
    [[ "$line" =~ ^[[:space:]]*image:[[:space:]]*(.+)$ ]] || continue
    ref=${BASH_REMATCH[1]}
    if [[ "$ref" =~ ^\$\{LC_IMAGE_PREFIX:-lc\}-(go|admin|storefront|caddy):\$\{IMAGE_TAG:\?[^}]*\}$ ]]; then
      continue
    fi
    [[ "$ref" =~ $digest ]] || bad "$rel" "image $ref is not digest-pinned"
    [[ "$ref" == *:latest* ]] && bad "$rel" "image uses :latest"
  done <"$cf"
done

if ((fail)); then exit 1; fi
echo "pin PASS all external images digest-pinned, no :latest"
