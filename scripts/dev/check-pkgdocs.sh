#!/usr/bin/env bash
# check-pkgdocs.sh — every Go package must carry a package comment that says what it OWNS and what it
# NEVER does (PROCESS.md §5, unit maintainability). Internal "depends on / used by" is not checked:
# docs/engineering/dependency-map.md is generated and CI-checked instead.
#
# Fails when: a package has no doc (`go list .Doc` empty), or its full doc lacks the word "owns" or the
# word "never" (case-insensitive). Prints every offender, exits 1 on any.
#
# Usage: bash scripts/dev/check-pkgdocs.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.1}"
bad=0
while read -r pkg doc; do
  if [[ -z "$doc" ]]; then echo "no package doc: $pkg" >&2; bad=1; continue; fi
  # `go doc` prints the whole package comment before the first exported declaration (commands have no
  # `package x // import` header line).
  full="$(go doc "$pkg" 2>/dev/null | awk '/^(func|type|const|var) /{exit} !/^package .* \/\/ import /{print}')"
  grep -qiE '(^|[^[:alnum:]_])owns([^[:alnum:]_]|$)' <<<"$full" || { echo "doc lacks \"owns\": $pkg" >&2; bad=1; }
  grep -qiE '(^|[^[:alnum:]_])never([^[:alnum:]_]|$)' <<<"$full" || { echo "doc lacks \"never\": $pkg" >&2; bad=1; }
done < <(go list -f '{{.ImportPath}} {{.Doc}}' ./...)
if [[ "$bad" == 1 ]]; then echo "check-pkgdocs: FAIL (PROCESS.md §5: Owns + Never in every package comment)" >&2; exit 1; fi
echo "check-pkgdocs: ok"
