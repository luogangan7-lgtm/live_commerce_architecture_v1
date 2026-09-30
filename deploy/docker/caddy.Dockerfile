# File: deploy/docker/caddy.Dockerfile
# Purpose: build "lc-caddy", the TLS edge. Stock Caddy with its file capability removed
#   (`setcap -r`) so it can exec under `cap_drop: [ALL]` + no-new-privileges as UID 10001.
#   Binding :80/:443 unprivileged works because the edge netns sets
#   net.ipv4.ip_unprivileged_port_start=0 (VERIFIED_LOCAL during design, deploy-design §22).
# Build: docker build -f deploy/docker/caddy.Dockerfile -t lc-caddy:<sha12> deploy/docker
#   (no repo files are copied; the Caddyfile is bind-mounted read-only at runtime).
# Runs as/in: service "caddy" in deploy/compose.yml, UID/GID 10001, read-only rootfs.
# Reads env (runtime): LC_ADMIN_HOST, LC_STORE_HOST, LC_API_HOST, LC_HOOKS_HOST (site names,
#   from compose.env), ACME_EMAIL and optional LC_ACME_CA (from env/caddy.env).
# Reads secrets: none (ACME account keys/certs live in the caddy-data volume at /data).
# Used by: deploy/compose.yml caddy; deploy/scripts/smoke.sh S05 (caddy validate/fmt), S25.
# Depends on: caddy:2.11.4-alpine (has busybox wget for the healthcheck, libcap setcap).
# Status: DESIGN (steps VERIFIED_LOCAL in design probe); verified by smoke S07, S08, S25.
# Change rules: digest bump -> smoke full; any plugin (rate limit/WAF) is a new dependency (O9).

ARG CADDY_IMAGE=caddy:2.11.4-alpine@sha256:6aeddd44c3078b0f9a35206472a11420648a79c184603ef95957d0a20044cb2b
FROM ${CADDY_IMAGE}
# /data = certificates + ACME account, /config = autosave, /run/caddy = admin unix socket (tmpfs).
RUN setcap -r /usr/bin/caddy \
 && addgroup -S -g 10001 lccaddy && adduser -S -D -H -u 10001 -G lccaddy lccaddy \
 && mkdir -p /data/caddy /config/caddy /run/caddy && chown -R 10001:10001 /data /config /run/caddy
USER 10001:10001
# Probes the loopback-only :2080 health site defined in deploy/caddy/Caddyfile.
HEALTHCHECK --interval=15s --timeout=3s --retries=4 CMD wget -q -O /dev/null http://127.0.0.1:2080/healthz || exit 1
ARG GIT_SHA=unknown
LABEL org.opencontainers.image.title="lc-caddy" \
      org.opencontainers.image.revision=$GIT_SHA \
      org.opencontainers.image.source="live_commerce_architecture_v1" \
      org.opencontainers.image.description="live-commerce TLS edge (Caddy, non-root, no file caps)"
