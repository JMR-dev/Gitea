# syntax=docker/dockerfile:1
#
# Caddy on a Debian 13 (trixie) base, built with xcaddy so two compile-time
# plugins are baked in:
#
#   googleclouddns  -- ACME DNS-01, so certificates never depend on inbound 80
#   coraza-caddy    -- OWASP Coraza WAF, with the Core Rule Set embedded
#
# The stock caddy binary can do neither: both are compile-time modules. The CRS
# itself needs no files on disk -- coraza-caddy's `load_owasp_crs` pulls in the
# coraza-coreruleset Go package, which embeds the rules in the binary.

ARG DEBIAN_TAG=13-slim
ARG GOLANG_TAG=1.26-trixie

# ---------------------------------------------------------------------------
# Stage 1: build caddy with the googleclouddns plugin.
# ---------------------------------------------------------------------------
FROM golang:${GOLANG_TAG} AS build

ARG CADDY_VERSION
ARG CORAZA_VERSION
ARG XCADDY_VERSION=latest

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

RUN set -eux; \
    test -n "${CADDY_VERSION}"  || { echo "CADDY_VERSION build-arg is required" >&2; exit 1; }; \
    test -n "${CORAZA_VERSION}" || { echo "CORAZA_VERSION build-arg is required" >&2; exit 1; }; \
    go install "github.com/caddyserver/xcaddy/cmd/xcaddy@${XCADDY_VERSION}"

RUN set -eux; \
    xcaddy build "v${CADDY_VERSION}" \
        --with github.com/caddy-dns/googleclouddns \
        --with "github.com/corazawaf/coraza-caddy/v2@${CORAZA_VERSION}" \
        --output /out/caddy; \
    /out/caddy version; \
    # Assert BOTH modules landed. It is easy to drop one when editing the build
    # line above, and a missing module fails at request time, not build time --
    # by which point it is a silently unprotected server or a broken cert renewal.
    /out/caddy list-modules > /tmp/modules.txt; \
    grep -q '^dns.providers.googleclouddns$' /tmp/modules.txt \
        || { echo "googleclouddns module missing"; cat /tmp/modules.txt; exit 1; }; \
    grep -qiE 'coraza|waf' /tmp/modules.txt \
        || { echo "coraza module missing"; cat /tmp/modules.txt; exit 1; }; \
    echo "WAF/DNS modules present:"; grep -iE 'coraza|waf|googleclouddns' /tmp/modules.txt

# ---------------------------------------------------------------------------
# Stage 2: runtime.
# ---------------------------------------------------------------------------
FROM debian:${DEBIAN_TAG}

ARG CADDY_VERSION

LABEL org.opencontainers.image.title="caddy-gitea" \
      org.opencontainers.image.description="Caddy with Google Cloud DNS ACME and the Coraza WAF, on a Debian 13 base" \
      org.opencontainers.image.version="${CADDY_VERSION}" \
      org.opencontainers.image.source="https://github.com/caddy-dns/googleclouddns" \
      org.opencontainers.image.base.name="docker.io/library/debian:13-slim"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 1000 caddy; \
    useradd --uid 1000 --gid 1000 --home-dir /config --shell /usr/sbin/nologin caddy; \
    mkdir -p /data /config; \
    chown -R 1000:1000 /data /config

COPY --from=build --chown=root:root --chmod=0755 /out/caddy /usr/local/bin/caddy

# Where caddy persists ACME account keys and issued certificates.
ENV XDG_DATA_HOME=/data \
    XDG_CONFIG_HOME=/config

USER 1000:1000
WORKDIR /config

EXPOSE 80 443 443/udp

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -fsS http://127.0.0.1:2019/config/ >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/caddy"]
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
