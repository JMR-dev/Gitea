# syntax=docker/dockerfile:1
#
# Gitea on a Debian 13 (trixie) base.
#
# Deliberately NOT the upstream image: that one is Alpine-based and its
# GITEA__section__KEY environment support comes from an `environment-to-ini`
# entrypoint helper, not from the gitea binary. We template app.ini on the host
# and bind-mount it read-only instead, so no such helper is needed here.

ARG DEBIAN_TAG=13-slim

# ---------------------------------------------------------------------------
# Stage 1: fetch and verify the release binary.
# The checksum alone proves nothing (it is served from the same place as the
# binary); the detached signature is the actual integrity guarantee.
# ---------------------------------------------------------------------------
FROM debian:${DEBIAN_TAG} AS fetch

ARG GITEA_VERSION
ARG GITEA_GPG_KEY=7C9E68152594688862D62AF62D9AE806EC1592E2
ARG TARGETARCH=amd64

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg xz-utils; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /tmp/gitea

RUN set -eux; \
    test -n "${GITEA_VERSION}" || { echo "GITEA_VERSION build-arg is required" >&2; exit 1; }; \
    base="https://github.com/go-gitea/gitea/releases/download/v${GITEA_VERSION}"; \
    file="gitea-${GITEA_VERSION}-linux-${TARGETARCH}.xz"; \
    curl -fsSL -o "${file}"        "${base}/${file}"; \
    curl -fsSL -o "${file}.sha256" "${base}/${file}.sha256"; \
    curl -fsSL -o "${file}.asc"    "${base}/${file}.asc"; \
    sha256sum -c "${file}.sha256"; \
    export GNUPGHOME="$(mktemp -d)"; \
    for ks in keys.openpgp.org keyserver.ubuntu.com pgp.mit.edu; do \
        gpg --batch --keyserver "hkps://${ks}" --recv-keys "${GITEA_GPG_KEY}" && break; \
    done; \
    gpg --batch --verify "${file}.asc" "${file}"; \
    gpgconf --kill all; \
    rm -rf "${GNUPGHOME}"; \
    xz -d "${file}"; \
    mv "gitea-${GITEA_VERSION}-linux-${TARGETARCH}" /tmp/gitea/gitea; \
    chmod 0755 /tmp/gitea/gitea; \
    /tmp/gitea/gitea --version

# ---------------------------------------------------------------------------
# Stage 2: runtime.
# ---------------------------------------------------------------------------
FROM debian:${DEBIAN_TAG}

ARG GITEA_VERSION

LABEL org.opencontainers.image.title="gitea" \
      org.opencontainers.image.description="Gitea on a Debian 13 base" \
      org.opencontainers.image.version="${GITEA_VERSION}" \
      org.opencontainers.image.source="https://github.com/go-gitea/gitea" \
      org.opencontainers.image.base.name="docker.io/library/debian:13-slim"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        git-lfs \
        openssh-client \
        tzdata; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd --gid 1000 git; \
    useradd --uid 1000 --gid 1000 --home-dir /var/lib/gitea --shell /bin/bash git; \
    mkdir -p /var/lib/gitea /etc/gitea; \
    chown -R 1000:1000 /var/lib/gitea

COPY --from=fetch --chown=root:root --chmod=0755 /tmp/gitea/gitea /usr/local/bin/gitea

ENV GITEA_WORK_DIR=/var/lib/gitea \
    GITEA_CUSTOM=/var/lib/gitea/custom

USER 1000:1000
WORKDIR /var/lib/gitea

# HTTP (behind Caddy) and the built-in SSH server.
EXPOSE 3000 2222

# Informational only -- the quadlet declares the authoritative healthcheck,
# because `Notify=healthy` needs it defined there to gate unit startup.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -fsS http://127.0.0.1:3000/api/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/gitea"]
CMD ["web", "--config", "/etc/gitea/app.ini"]
