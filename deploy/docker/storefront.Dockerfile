# File: deploy/docker/storefront.Dockerfile
# Purpose: build "lc-storefront", the buyer site (apps/storefront). It is NOT a Next standalone
#   build (no `output` in apps/storefront/next.config.ts), so the image carries the pruned pnpm
#   workspace (prod node_modules + .next) and runs the exact tested `next start` command.
#   Moving to standalone is REQUIRES_INTEGRATOR (I3; apps/** is outside deploy write paths).
# Build: docker build -f deploy/docker/storefront.Dockerfile -t lc-storefront:<sha12> .
# Runs as/in: build host; runtime user 1000, read-only rootfs (+ tmpfs .next/cache).
# Reads env (build): COMMERCE_BUYER_WEB_ENABLED=0 (tested build, scripts/dev/test-local.sh).
# Reads env (runtime, wired in deploy/compose.yml): COMMERCE_BUYER_WEB_ENABLED,
#   COMMERCE_BUYER_API_ORIGIN=http://127.0.0.1:8080, COMMERCE_BUYER_SESSION_TTL (integer seconds;
#   apps/storefront/lib/buyer-server.ts:118-121).
# Reads secrets: commerce_buyer_bff_key (must equal the API's), commerce_buyer_cookie_key
#   (must differ from the BFF key) via *_FILE, expanded by lcentry.
# Used by: deploy/compose.yml service "storefront" (edge netns; Caddy proxies :3200).
# Depends on: pnpm workspace manifests, apps/storefront, packages/i18n (workspace link),
#   deploy/tools/lcentry.
# Status: DESIGN/NOT_RUN; verified by smoke S07, S08, S20.
#   V1: `next start` must load next.config.ts without the root `typescript` devDependency.
#   If S20 fails for that reason, the documented fallback is: replace the two prod-deps COPY
#   lines with `COPY --from=deps /app/node_modules ./node_modules` and
#   `COPY --from=deps /app/apps/storefront/node_modules ./apps/storefront/node_modules`
#   (bigger image, same tested layout) and record which variant the evidence used.
# Change rules: keep the CMD identical to tests/storefront/browser-gate.mjs launch (127.0.0.1).

ARG GO_IMAGE=golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183
ARG NODE_IMAGE=node:24.15.0-trixie-slim@sha256:291be77873bc04731968cacf82f0fcef17cee8cf200c6b6951e2bcab41560eb7

FROM ${GO_IMAGE} AS lcentry
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath GOPROXY=off
WORKDIR /src
COPY go.mod go.sum ./
COPY deploy/tools/lcentry ./deploy/tools/lcentry
RUN go build -buildvcs=false -ldflags=-buildid= -o /out/lcentry ./deploy/tools/lcentry

FROM ${NODE_IMAGE} AS base
ENV PNPM_HOME=/pnpm CI=1 NEXT_TELEMETRY_DISABLED=1
ENV PATH=$PNPM_HOME:$PATH
RUN corepack enable && corepack prepare pnpm@10.33.0 --activate
WORKDIR /app
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/admin/package.json apps/admin/
COPY apps/storefront/package.json apps/storefront/
COPY packages/i18n/package.json packages/i18n/

FROM base AS deps
RUN --mount=type=cache,id=pnpm-store,target=/pnpm/store pnpm install --frozen-lockfile --store-dir /pnpm/store

FROM deps AS build
COPY packages ./packages
COPY apps/storefront ./apps/storefront
# Tested build; drop the build cache so it is not shipped.
RUN COMMERCE_BUYER_WEB_ENABLED=0 pnpm run build:storefront && rm -rf apps/storefront/.next/cache

FROM base AS prod-deps
# Same manifests as deps; production dependencies only (no typescript/playwright/prettier).
RUN --mount=type=cache,id=pnpm-store,target=/pnpm/store pnpm install --frozen-lockfile --prod --store-dir /pnpm/store

FROM ${NODE_IMAGE}
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1
WORKDIR /app
COPY package.json pnpm-workspace.yaml ./
# pnpm's symlinked layout is preserved by COPY --from (node_modules/.pnpm + per-app links).
COPY --from=prod-deps /app/node_modules ./node_modules
COPY --from=prod-deps /app/apps/storefront/node_modules ./apps/storefront/node_modules
# Workspace package @live-commerce/i18n is linked from apps/storefront/node_modules.
COPY --from=build /app/packages/i18n ./packages/i18n
COPY --from=build /app/apps/storefront/package.json /app/apps/storefront/next.config.ts ./apps/storefront/
COPY --from=build /app/apps/storefront/.next ./apps/storefront/.next
COPY --from=lcentry /out/lcentry /usr/local/bin/lcentry
USER 1000:1000
WORKDIR /app/apps/storefront
ENTRYPOINT ["/usr/local/bin/lcentry","run","--"]
# Same launch as tests/storefront/browser-gate.mjs:33; loopback only (edge netns, Caddy in front).
CMD ["/usr/local/bin/node","node_modules/next/dist/bin/next","start","--hostname","127.0.0.1","--port","3200"]
ARG GIT_SHA=unknown
LABEL org.opencontainers.image.title="lc-storefront" \
      org.opencontainers.image.revision=$GIT_SHA \
      org.opencontainers.image.source="live_commerce_architecture_v1" \
      org.opencontainers.image.description="live-commerce buyer storefront (next start)"
