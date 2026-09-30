# File: deploy/docker/admin.Dockerfile
# Purpose: build "lc-admin", the merchant back office (apps/admin) as a Next.js standalone server.
# Build: docker build -f deploy/docker/admin.Dockerfile -t lc-admin:<sha12> .   (context = repo root)
# Runs as/in: build host; runtime user 1000 ("node" in the base image), read-only rootfs.
# Reads env (build): COMMERCE_IDENTITY_ENABLED=0 COMMERCE_FIXTURE_ENABLED=0 are forced for the
#   build exactly like scripts/dev/test-local.sh (the tested build never needs an IdP or DB).
# Reads env (runtime, wired in deploy/compose.yml): HOSTNAME=localhost and PORT=3100 (listener;
#   Docker would otherwise set HOSTNAME to the container id; "localhost" rather than 127.0.0.1 so
#   Next relativises same-origin redirects, see compose.yml admin), COMMERCE_IDENTITY_ENABLED,
#   COMMERCE_PUBLIC_ORIGIN, COMMERCE_API_ORIGIN, COMMERCE_OIDC_ISSUER (optional when COMMERCE_PASSWORD_LOGIN_ENABLED=1), COMMERCE_PASSWORD_LOGIN_ENABLED, COMMERCE_ONBOARDING_*,
#   COMMERCE_FIXTURE_ENABLED=0 (apps/admin/lib/auth.ts, lib/backend.ts).
# Reads secrets: /run/secrets/commerce_bff_key via COMMERCE_BFF_KEY_FILE, expanded by lcentry.
#   authConfig is evaluated at server module load (runtime), never at build time.
# Used by: deploy/compose.yml service "admin" (shares the edge netns; Caddy proxies :3100).
# Depends on: package.json/pnpm-lock.yaml/pnpm-workspace.yaml (pnpm@10.33.0 via corepack),
#   apps/admin, packages/*, scripts/dev/package-admin.mjs (MANDATORY: standalone output drops
#   public/ and .next/static; that script copies them back), deploy/tools/lcentry.
# Status: DESIGN/NOT_RUN in this package; verified by smoke S07, S08, S21.
# Change rules: Node bump (O6: 24.15.0 -> 24.21.0) must re-run the repo browser gates and
#   smoke full; keep the digest pin; never COPY env/secret files into any stage.

# Same Go pin as go.Dockerfile; only used to compile the static lcentry launcher.
ARG GO_IMAGE=golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183
# Node 24.15.0 = ledger-tested toolchain (docs/implementation/dependencies.md:226); resolved 2026-09-28.
ARG NODE_IMAGE=node:24.15.0-trixie-slim@sha256:291be77873bc04731968cacf82f0fcef17cee8cf200c6b6951e2bcab41560eb7

FROM ${GO_IMAGE} AS lcentry
# GOPROXY=off proves lcentry is stdlib-only (no module download needed).
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath GOPROXY=off
WORKDIR /src
COPY go.mod go.sum ./
COPY deploy/tools/lcentry ./deploy/tools/lcentry
RUN go build -buildvcs=false -ldflags=-buildid= -o /out/lcentry ./deploy/tools/lcentry

FROM ${NODE_IMAGE} AS base
ENV PNPM_HOME=/pnpm CI=1 NEXT_TELEMETRY_DISABLED=1
ENV PATH=$PNPM_HOME:$PATH
# pnpm version comes from root package.json "packageManager": "pnpm@10.33.0".
RUN corepack enable && corepack prepare pnpm@10.33.0 --activate
WORKDIR /app

FROM base AS deps
# Manifests only, so the install layer is reused until a lockfile/manifest changes.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/admin/package.json apps/admin/
COPY apps/storefront/package.json apps/storefront/
COPY packages/i18n/package.json packages/i18n/
RUN --mount=type=cache,id=pnpm-store,target=/pnpm/store pnpm install --frozen-lockfile --store-dir /pnpm/store

FROM deps AS build
COPY packages ./packages
COPY apps/admin ./apps/admin
COPY scripts/dev/package-admin.mjs ./scripts/dev/package-admin.mjs
# Exact tested build (scripts/dev/test-local.sh): `next build && node ../../scripts/dev/package-admin.mjs`.
RUN COMMERCE_IDENTITY_ENABLED=0 COMMERCE_FIXTURE_ENABLED=0 pnpm run build:admin

FROM ${NODE_IMAGE}
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1
# Standalone tree: /app/apps/admin/server.js + traced /app/node_modules. Root-owned, read-only to UID 1000.
COPY --from=build /app/apps/admin/.next/standalone/ /app/
COPY --from=lcentry /out/lcentry /usr/local/bin/lcentry
USER 1000:1000
WORKDIR /app/apps/admin
ENTRYPOINT ["/usr/local/bin/lcentry","run","--"]
# Run server.js directly: scripts/dev/start-admin.mjs is local-only by its own comment.
# --dns-result-order=ipv4first: HOSTNAME=localhost (compose.yml) must bind 127.0.0.1, the address
#   Caddy and the healthcheck dial. Node's default "verbatim" order could pick ::1 on hosts with
#   IPv6 loopback, and the edge would then get "connection refused". The flag is here and not in
#   NODE_OPTIONS because admin.env owns NODE_OPTIONS (operator knob, preflight P06).
CMD ["/usr/local/bin/node","--dns-result-order=ipv4first","server.js"]
ARG GIT_SHA=unknown
LABEL org.opencontainers.image.title="lc-admin" \
      org.opencontainers.image.revision=$GIT_SHA \
      org.opencontainers.image.source="live_commerce_architecture_v1" \
      org.opencontainers.image.description="live-commerce merchant admin (Next.js standalone)"
