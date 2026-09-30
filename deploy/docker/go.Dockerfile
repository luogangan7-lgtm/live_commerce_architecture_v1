# File: deploy/docker/go.Dockerfile
# Purpose: build the single multi-binary Go image "lc-go" (架构.md §4.2: API and
#   workers share one image; compose picks the binary with `command:`).
# Build: docker build -f deploy/docker/go.Dockerfile -t lc-go:<sha12> .   (context = repo root;
#   normally via deploy/scripts/build-images.sh which also sets GIT_SHA).
# Runs as/in: build host (Docker >= 25, BuildKit cache mounts). Runtime user 65532 (distroless nonroot).
# Reads env (build): none. Build args: GO_IMAGE / RUNTIME_IMAGE (digest pins), GO_CMDS, GIT_SHA.
# Reads secrets: none at build time. Runtime secrets arrive as /run/secrets/* files and are
#   expanded by /app/bin/lcentry (deploy/tools/lcentry) — never baked into the image.
# Contents: /app/bin/{api,payment-worker,expiry-worker,meta-worker,claims-worker,ads-worker,media-worker,migrate,
#   stripe-admin,meta-admin,retention-admin,lcentry}. stripe-admin/meta-admin are OPERATOR CLIs: present in the image so
#   `ops` one-shots can run them, but no long-running service is given their registrar logins or inputs.
#   retention-admin (U08): the deploy post-check runs only `status` with the claims-worker's retention-job
#   login; the operator login never reaches the deploy host (claims-retention-purge-v1 §10(5)).
# Used by: deploy/compose.yml services api, expiry-worker, payment-worker-{sandbox,live},
#   meta-worker, claims-worker, ads-worker (profile ads), migrate, stripe-admin, meta-admin. media-worker is built but NOT deployed (MOCK-only, worker_env.go:174).
# Depends on: go.mod/go.sum (module "livecommerce", go 1.27.1), cmd/**, internal/**,
#   migrations/** (embedded SQL), deploy/tools/lcentry. `cmd/migrate` is REQUIRES_INTEGRATOR (I1):
#   until it exists this build stops with exit 3 "BLOCKED" instead of shipping an image without it.
# Status: DESIGN (NOT_RUN: blocked on cmd/migrate); verified by smoke S07, S08.
# Change rules: digest bump -> re-resolve with `docker buildx imagetools inspect`, update the
#   ledger in deploy/README.md, re-run `deploy/scripts/smoke.sh full`. Never add `-s -w`
#   (keep symbols for diagnosable stacks). Never add a HEALTHCHECK here (workers have none).
#   smoke S08 reads the expected binary list from the one-line `ARG GO_CMDS="..."` below (+ lcentry):
#   keep that line format, or S08 fails.

# golang 1.27.1 (matches go.mod `go 1.27.1`), Debian 13; resolved 2026-09-28.
ARG GO_IMAGE=golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183
# distroless static nonroot (UID 65532): CA certs for OIDC/PAYUNi TLS + tzdata, no shell; resolved 2026-09-28.
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

FROM ${GO_IMAGE} AS build
# CGO off -> fully static binaries for distroless/static; GOTOOLCHAIN=local forbids silent
# toolchain downloads (the image IS the pinned toolchain); -trimpath drops host paths.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
# Module download layer is cached independently of source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY deploy/tools ./deploy/tools
# `migrate` is required for any deploy (the only production caller of migrations.Apply).
ARG GO_CMDS="api payment-worker expiry-worker meta-worker claims-worker ads-worker media-worker migrate stripe-admin meta-admin retention-admin"
# -buildvcs=false: .git is excluded by .dockerignore; the revision goes into the OCI label.
# -ldflags=-buildid= : reproducible output for identical inputs.
# Every cmd/<name> is checked BEFORE anything compiles, so a missing one (I1: cmd/migrate) fails
# in seconds with exit 3 instead of after five full builds (VERIFIED_LOCAL 2026-09-28).
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    for c in $GO_CMDS; do \
      test -d "cmd/$c" || { echo "BLOCKED: cmd/$c missing (REQUIRES_INTEGRATOR)"; exit 3; }; \
    done; \
    for c in $GO_CMDS; do \
      go build -buildvcs=false -ldflags=-buildid= -o "/out/$c" "./cmd/$c"; \
    done; \
    go build -buildvcs=false -ldflags=-buildid= -o /out/lcentry ./deploy/tools/lcentry

FROM ${RUNTIME_IMAGE}
# Binaries are root-owned and read-only to UID 65532; the container rootfs is read-only anyway.
COPY --from=build /out/ /app/bin/
USER 65532:65532
WORKDIR /app
# lcentry converts NAME_FILE secrets into NAME and execve()s the command, so PID 2 (under
# compose `init: true`) is the real binary and /proc/self/exe points at it.
ENTRYPOINT ["/app/bin/lcentry","run","--"]
CMD ["/app/bin/api"]
ARG GIT_SHA=unknown
LABEL org.opencontainers.image.title="lc-go" \
      org.opencontainers.image.revision=$GIT_SHA \
      org.opencontainers.image.source="live_commerce_architecture_v1" \
      org.opencontainers.image.description="live-commerce Go API + workers + migrate (select with command)"
