#!/usr/bin/env bash
# File: deploy/scripts/build-images.sh
# Purpose: build the four deploy images, tagged with the 12-char commit (+ "-dirty" when the
#   checkout has local changes) and OCI revision labels: lc-go, lc-admin, lc-storefront, lc-caddy.
# Usage: build-images.sh [--tag TAG] [--only go|admin|storefront|caddy]... [--evidence DIR]
# Runs as/in: build host with Docker >= 25 (BuildKit cache mounts). Context = repo root
#   (.dockerignore filters it); caddy uses deploy/docker as context.
# Reads env: LC_IMAGE_PREFIX (default lc); HTTP_PROXY/HTTPS_PROXY/NO_PROXY are passed as build
#   args only when set (Docker predefined args; not persisted in image config).
#   LC_BUILD_NETWORK = auto (default) | default | host: network of the RUN steps. BuildKit runs
#   them in their own network namespace, where a proxy on the build host's LOOPBACK
#   (http://127.0.0.1:PORT, localhost, [::1]) is unreachable (VERIFIED_LOCAL 2026-09-28:
#   ECONNREFUSED; builds only worked because the needed hosts were in NO_PROXY). "auto" therefore
#   builds with `--network host` exactly when a forwarded proxy variable points at loopback, and
#   says so; otherwise the default isolated build network is kept. The network choice affects only
#   the build, never the image.
# Reads secrets: none — images never contain secrets or env files.
# Used by: operators before deploy.sh (printed tag -> deploy.sh first|upgrade <tag>, which records it
#   in compose.env), smoke.sh S07.
# Depends on: deploy/docker/*.Dockerfile, git (commit id), docker.
# Exit: 0 built, 1 build failed, 3 BLOCKED (cmd/migrate missing; kept as a guard, it exists since I1 closed:
#   the Go image would be undeployable without it, so nothing is faked).
# Status: DESIGN; all four images build and pass smoke S07/S08 (R1, Linux dind run recorded in
#   deploy/README.md). lc-go now also carries claims-worker, ads-worker, stripe-admin and meta-admin.
# Change rules: keep tags immutable (never reuse a tag for different content; "-dirty" tags are
#   for rehearsal only and must not be deployed to production).
set -Eeuo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

tag="" only=() evidence=""
while (($#)); do
  case "$1" in
  --tag)
    tag=${2:?}
    shift 2
    ;;
  --only)
    only+=("${2:?}")
    shift 2
    ;;
  --evidence)
    evidence=${2:?}
    shift 2
    ;;
  *) lc_die "usage: build-images.sh [--tag TAG] [--only go|admin|storefront|caddy]... [--evidence DIR]" 2 ;;
  esac
done
((${#only[@]})) || only=(go admin storefront caddy)
tag=${tag:-$(lc_git_tag)}
[[ "$tag" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || lc_die "invalid tag"
sha=$(git -C "$LC_REPO_ROOT" rev-parse HEAD)
prefix=${LC_IMAGE_PREFIX:-lc}

for img in "${only[@]}"; do
  if [[ "$img" == go && ! -d "$LC_REPO_ROOT/cmd/migrate" ]]; then
    lc_error "BLOCKED: cmd/migrate is missing (REQUIRES_INTEGRATOR I1, deploy-design §7); lc-go would be undeployable"
    exit 3
  fi
done

proxy_args=() loopback_proxy=""
# Optional base-image overrides (GO_IMAGE / NODE_IMAGE / RUNTIME_IMAGE build args). Leave unset in
# production so the digest pins in deploy/docker/*.Dockerfile apply. Only for build hosts behind a
# TLS-intercepting proxy, where a locally derived base image adds that proxy's CA (the cloud dev
# container; see docs/runbooks/deploy.md). The value is an image reference, never a secret.
for v in GO_IMAGE NODE_IMAGE RUNTIME_IMAGE; do
  [[ -n "${!v:-}" ]] || continue
  lc_warn "base image override: $v=${!v} (not the pinned digest)"
  proxy_args+=(--build-arg "$v=${!v}")
done
for v in HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy; do
  [[ -n "${!v:-}" ]] || continue
  proxy_args+=(--build-arg "$v")
  # Proxy URL host = text after "scheme://" and optional "user:pass@", up to ":port" or "/".
  # Only the variable NAME is logged, never the value (it may carry credentials).
  if [[ "$v" != [Nn][Oo]_* && "${!v}" =~ ^[A-Za-z0-9+.-]+://([^/@]*@)?(\[::1\]|localhost|127\.[0-9.]+)(:|/|$) ]]; then
    loopback_proxy+=" $v"
  fi
done
net_args=()
case "${LC_BUILD_NETWORK:-auto}" in
auto)
  if [[ -n "$loopback_proxy" ]]; then
    net_args=(--network host)
    lc_warn "proxy variable(s)${loopback_proxy} point at host loopback, unreachable from the isolated build network: building with --network host (LC_BUILD_NETWORK=default keeps the isolated network)"
  fi
  ;;
host) net_args=(--network host) ;;
default)
  [[ -z "$loopback_proxy" ]] || lc_warn "proxy variable(s)${loopback_proxy} point at host loopback and LC_BUILD_NETWORK=default: RUN steps cannot reach that proxy"
  ;;
*) lc_die "LC_BUILD_NETWORK must be auto, default or host" 2 ;;
esac

build() { # name dockerfile context
  lc_info "building $prefix-$1:$tag${net_args[*]:+ (${net_args[*]})}"
  docker build --pull=false "${net_args[@]}" -f "$2" -t "$prefix-$1:$tag" --build-arg "GIT_SHA=$sha" "${proxy_args[@]}" "$3"
}
for img in "${only[@]}"; do
  case "$img" in
  go) build go "$LC_DEPLOY_DIR/docker/go.Dockerfile" "$LC_REPO_ROOT" ;;
  admin) build admin "$LC_DEPLOY_DIR/docker/admin.Dockerfile" "$LC_REPO_ROOT" ;;
  storefront) build storefront "$LC_DEPLOY_DIR/docker/storefront.Dockerfile" "$LC_REPO_ROOT" ;;
  caddy) build caddy "$LC_DEPLOY_DIR/docker/caddy.Dockerfile" "$LC_DEPLOY_DIR/docker" ;;
  *) lc_die "unknown image $img" 2 ;;
  esac
done

report=$(for img in "${only[@]}"; do
  docker image inspect "$prefix-$img:$tag" --format \
    '{"image":"{{index .RepoTags 0}}","id":"{{.Id}}","size":{{.Size}},"user":"{{.Config.User}}","revision":"{{index .Config.Labels "org.opencontainers.image.revision"}}"}'
done)
printf '%s\n' "$report"
if [[ -n "$evidence" ]]; then
  mkdir -p "$evidence"
  printf '%s\n' "$report" >"$evidence/images.jsonl"
fi
lc_info "IMAGE_TAG=$tag (deploy with: deploy.sh first $tag | deploy.sh upgrade $tag; deploy.sh writes it into compose.env)"
