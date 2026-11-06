# syntax=docker/dockerfile:1
#
# Gitea, rootless, on an Alpine base.
#
# Our own image rather than FROM the upstream gitea image. Upstream's rootless
# variant (Dockerfile.rootless) is the reference for what Gitea needs at
# runtime, and this takes that and nothing more:
#
#   taken    Alpine, git, curl, ca-certificates, openssh-keygen, dumb-init as
#            PID 1, user git 1000:1000, the built-in SSH server (no sshd, no s6)
#   taken    bash. Not for upstream's scripts: Gitea's server-side git hooks are
#            `#!/usr/bin/env bash` ([repository] SCRIPT_TYPE defaults to bash),
#            and every existing repository already carries hooks written that
#            way. Without bash every push is rejected ("pre-receive hook
#            declined") -- verified, not theoretical.
#   dropped  docker-entrypoint.sh / docker-setup.sh / environment-to-ini. We
#            template app.ini on the host and bind-mount it read-only, and those
#            scripts are exactly what fails on a read-only app.ini.
#   dropped  gettext and gnupg: only those scripts (envsubst) and Gitea's own
#            commit signing use them, and we use neither.
#   dropped  HOME=/var/lib/gitea/git, TMPDIR=/tmp/gitea and the VOLUMEs. The host
#            contract below is the one the Debian image had, so nothing on the
#            VM (quadlet, vm/bootstrap.sh) has to change.
#
# Host contract -- vm/bootstrap.sh and vm/quadlets/gitea.container rely on it:
#   * `IMAGE generate secret KEY` runs gitea   (bootstrap.sh's secret safety net)
#   * `--entrypoint curl IMAGE ...` works      (bootstrap.sh's Caddy network probe)
#   * `gitea` is on PATH at /usr/local/bin     (`podman exec gitea gitea dump`)
#   * uid 1000, HOME and GITEA_WORK_DIR = /var/lib/gitea, no image volumes
#   * curl in the image, for the quadlet's HealthCmd
#   * rsync, unzip, sqlite3 in the image, for the release engine's restores
#
# GITEA_VERSION is a full x.y.z, resolved from the major line in
# image/gitea.version by the build (cloudbuild/image.yaml); this file never
# picks a version itself.

ARG ALPINE_TAG=3.24

# ---------------------------------------------------------------------------
# Stage 1: fetch and verify the release binary.
# The checksum alone proves nothing (it is served from the same place as the
# binary); the detached signature is the actual integrity guarantee. That is
# also what authenticates a version the build resolved on its own.
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_TAG} AS fetch

ARG GITEA_VERSION
# Teabot <teabot@gitea.io>. Releases are signed by its signing subkey
# CC64B1DB67ABBEECAB24B6455FC346329753F4B0. The key expires 2027-06-26: when
# gpg starts reporting an expired key, re-fetch it (Gitea extends it) before
# suspecting the release.
ARG GITEA_GPG_KEY=7C9E68152594688862D62AF62D9AE806EC1592E2
ARG TARGETARCH=amd64

RUN set -eux; \
    apk add --no-cache ca-certificates curl gnupg xz

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
FROM alpine:${ALPINE_TAG}

ARG ALPINE_TAG
ARG GITEA_VERSION

LABEL org.opencontainers.image.title="gitea" \
      org.opencontainers.image.description="Gitea, rootless, on an Alpine base" \
      org.opencontainers.image.version="${GITEA_VERSION}" \
      org.opencontainers.image.source="https://github.com/go-gitea/gitea" \
      org.opencontainers.image.base.name="docker.io/library/alpine:${ALPINE_TAG}" \
      dev.jasonmross.alpine.minor="${ALPINE_TAG}"

# apk upgrade: the tag floats within the Alpine minor, but a cached base layer
# does not, so the weekly rebuild upgrades explicitly.
#
# dumb-init is PID 1 so orphaned git and ssh grandchildren get reaped; Gitea
# itself only waits on its own children. git-lfs and openssh-client keep the
# package set the Debian image had (LFS migrations, SSH mirrors).
#
# rsync, unzip and sqlite are not for Gitea: the image doubles as the toolbox
# for restores (scripts/ops/ansible/files/gitea-ops-lib.sh), because the host
# has none of them.
# Info-ZIP unzip rather than busybox's, which cannot read zip64 -- and `gitea
# dump` switches to zip64 once a dump passes 4 GB.
RUN set -eux; \
    apk upgrade --no-cache; \
    apk add --no-cache \
        bash \
        ca-certificates \
        curl \
        dumb-init \
        git \
        git-lfs \
        openssh-client \
        openssh-keygen \
        rsync \
        sqlite \
        tzdata \
        unzip; \
    addgroup -S -g 1000 git; \
    adduser -S -D -H -h /var/lib/gitea -s /bin/sh -u 1000 -G git git; \
    mkdir -p /var/lib/gitea /etc/gitea; \
    chown 1000:1000 /var/lib/gitea

COPY --from=fetch --chown=root:root --chmod=0755 /tmp/gitea/gitea /usr/local/bin/gitea

# The release binary is pure Go (CGO off, modernc SQLite), so it runs on musl.
# Prove it here rather than in a crash loop on the VM.
RUN /usr/local/bin/gitea --version

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

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/gitea"]
CMD ["web", "--config", "/etc/gitea/app.ini"]
