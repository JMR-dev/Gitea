#!/usr/bin/env bash
#
# Gitea VM bootstrap. Set as the GCE `startup-script` metadata value by Pulumi,
# and re-run by gitea-config-sync.service with --sync-only after a config push.
#
# MUST be idempotent: GCE runs the startup script on every boot.
#
#   (no args)     full run -- packages, disk, SELinux, firewall, units, config
#   --sync-only   re-pull vm/ from GCS, re-render templates, restart what changed
#
set -euo pipefail

readonly STATE_DIR=/opt/gitea-config
readonly RENDER_DIR=/etc/containers/systemd
readonly LOG_TAG=gitea-bootstrap

log()  { echo "[${LOG_TAG}] $*" >&2; }
die()  { echo "[${LOG_TAG}] FATAL: $*" >&2; exit 1; }
warn() { echo "[${LOG_TAG}] WARN: $*" >&2; }

MODE=full
[[ "${1:-}" == "--sync-only" ]] && MODE=sync

# ---------------------------------------------------------------------------
# Instance metadata (populated by Pulumi)
# ---------------------------------------------------------------------------
meta() {
    curl -fsS -H 'Metadata-Flavor: Google' \
        "http://169.254.169.254/computeMetadata/v1/instance/attributes/$1" 2>/dev/null || true
}

CONFIG_BUCKET=$(meta config-bucket)
BACKUP_BUCKET=$(meta backup-bucket)
GCP_PROJECT=$(meta gcp-project)
AR_HOST=$(meta ar-host)
IMAGE_GITEA=$(meta image-gitea)
IMAGE_CADDY=$(meta image-caddy)
DOMAIN=$(meta domain)
ACME_EMAIL=$(meta acme-email)
APP_NAME=$(meta app-name)
PODMAN_SUBNET=$(meta podman-subnet)
PODMAN_GATEWAY=$(meta podman-gateway)
REQUIRE_SIGNIN_VIEW=$(meta require-signin-view)
WAF_MODE=$(meta waf-mode)
DATA_DISK_DEVICE=$(meta data-disk-device)

[[ -n "${CONFIG_BUCKET}" ]] || die "config-bucket metadata is missing; nothing to sync from"
[[ -n "${DOMAIN}"        ]] || die "domain metadata is missing"
: "${APP_NAME:=Gitea}"
: "${REQUIRE_SIGNIN_VIEW:=false}"
# DetectionOnly is the safe default: run it, read what it flags, add
# exclusions, then switch to On. See docs/waf.md.
: "${WAF_MODE:=DetectionOnly}"
case "${WAF_MODE}" in
    On|DetectionOnly|Off) ;;
    *) die "waf-mode must be On, DetectionOnly, or Off (got: ${WAF_MODE})" ;;
esac
: "${DATA_DISK_DEVICE:=/dev/disk/by-id/google-gitea-data}"

export GCP_PROJECT AR_HOST IMAGE_GITEA IMAGE_CADDY DOMAIN ACME_EMAIL APP_NAME
export PODMAN_SUBNET PODMAN_GATEWAY REQUIRE_SIGNIN_VIEW WAF_MODE

# ---------------------------------------------------------------------------
# Packages
# ---------------------------------------------------------------------------
install_packages() {
    log "installing packages"
    dnf -y install \
        podman container-selinux \
        nftables \
        jq gettext \
        policycoreutils-python-utils \
        xfsprogs

    # fail2ban lives in EPEL on RHEL-family distros. The exact package set has
    # shifted between EPEL releases, so probe rather than assume -- this is the
    # one dependency most likely to be named differently on EPEL 10.
    if ! rpm -q epel-release >/dev/null 2>&1; then
        dnf -y install epel-release || warn "epel-release unavailable; fail2ban will be skipped"
    fi
    if dnf -y install fail2ban fail2ban-server 2>/dev/null; then
        # The systemd journal backend needs the Python bindings; without them
        # fail2ban silently falls back and matches nothing.
        dnf -y install python3-systemd || warn "python3-systemd missing; the systemd backend may not work"
    else
        warn "fail2ban not installable from configured repos -- skipping fail2ban setup"
    fi
}

# ---------------------------------------------------------------------------
# Data disk
# ---------------------------------------------------------------------------
setup_data_disk() {
    log "configuring data disk ${DATA_DISK_DEVICE}"
    [[ -e "${DATA_DISK_DEVICE}" ]] || die "data disk ${DATA_DISK_DEVICE} not present"

    if ! blkid "${DATA_DISK_DEVICE}" >/dev/null 2>&1; then
        log "disk is unformatted -- creating XFS filesystem"
        mkfs.xfs -q "${DATA_DISK_DEVICE}"
    fi

    local uuid
    uuid=$(blkid -s UUID -o value "${DATA_DISK_DEVICE}")
    [[ -n "${uuid}" ]] || die "could not read UUID from ${DATA_DISK_DEVICE}"

    mkdir -p /var/lib/gitea

    # By UUID, never by device path: GCE can reorder /dev/sdX across reboots.
    if ! grep -q "UUID=${uuid}" /etc/fstab; then
        log "adding fstab entry for ${uuid}"
        printf 'UUID=%s /var/lib/gitea xfs defaults,nofail,x-systemd.device-timeout=30 0 2\n' \
            "${uuid}" >> /etc/fstab
    fi

    systemctl daemon-reload
    mountpoint -q /var/lib/gitea || mount /var/lib/gitea
    mountpoint -q /var/lib/gitea || die "/var/lib/gitea failed to mount"

    # Set the SELinux label persistently ONCE, rather than putting :Z on the
    # quadlet's volume line. :Z would force a recursive relabel of the entire
    # repository tree on every container start.
    if ! semanage fcontext -l 2>/dev/null | grep -q '^/var/lib/gitea(/\.\*)?'; then
        log "setting persistent SELinux fcontext on /var/lib/gitea"
        semanage fcontext -a -t container_file_t '/var/lib/gitea(/.*)?' || \
            warn "semanage fcontext failed; check SELinux state"
    fi
    restorecon -RF /var/lib/gitea || warn "restorecon failed"

    # The mount point itself must belong to the container's uid, not just the
    # subdirectories: Gitea creates GITEA_CUSTOM (/var/lib/gitea/custom) at
    # startup, and a root-owned 0755 mount point makes that mkdir fail with a
    # bare "permission denied" that reads like an SELinux problem.
    chown 1000:1000 /var/lib/gitea
    chmod 0750 /var/lib/gitea
    install -d -o 1000 -g 1000 -m 0750 \
        /var/lib/gitea/data /var/lib/gitea/log /var/lib/gitea/custom
}

# ---------------------------------------------------------------------------
# Swap
# ---------------------------------------------------------------------------
# GCE instances ship with no swap. On e2-small (2 GB) that is a real risk: a
# steady-state Gitea + Caddy/Coraza pair measures ~155 MB, but git subprocesses
# spawned during a push (git-receive-pack, index-pack, gc) push the total past
# 400 MB on a modest repo and scale with repo size. Without swap, the OOM killer
# picks a victim mid-push.
#
# This is ballast, not working memory -- hence the low swappiness. If the box is
# swapping steadily, the answer is a bigger machine type, not more swap.
setup_swap() {
    local swapfile=/swapfile size_mb=2048

    if swapon --show=NAME --noheadings 2>/dev/null | grep -qx "${swapfile}"; then
        log "swap already active"
    else
        if [[ ! -f "${swapfile}" ]]; then
            log "creating ${size_mb}MB swap file"
            # dd, not fallocate: a fallocated file can carry unwritten extents
            # that mkswap accepts and the kernel then refuses to swap to.
            dd if=/dev/zero of="${swapfile}" bs=1M count="${size_mb}" status=none
            chmod 0600 "${swapfile}"
            mkswap "${swapfile}" >/dev/null
        fi
        swapon "${swapfile}" || warn "swapon failed"
    fi

    grep -q "^${swapfile} " /etc/fstab \
        || printf '%s none swap sw 0 0\n' "${swapfile}" >> /etc/fstab

    echo 'vm.swappiness = 10' > /etc/sysctl.d/90-gitea-swappiness.conf
    sysctl -q -p /etc/sysctl.d/90-gitea-swappiness.conf || warn "could not apply swappiness"
}

# ---------------------------------------------------------------------------
# Podman / netavark
# ---------------------------------------------------------------------------
configure_podman() {
    log "configuring podman firewall driver"
    mkdir -p /etc/containers/containers.conf.d
    # Netavark keeps its rules in a dedicated `netavark` nftables table, which is
    # what makes coexistence with our own table workable. Changing this with
    # containers running leaves conflicting rules behind -- it is set here,
    # before anything starts, and a reboot is the documented fix if it is ever
    # changed on a live host.
    cat > /etc/containers/containers.conf.d/10-gitea.conf <<'EOF'
[network]
firewall_driver = "nftables"
EOF
}

# ---------------------------------------------------------------------------
# Host firewall
# ---------------------------------------------------------------------------
# Runs on every invocation so a pushed vm/nftables/gitea.nft change applies
# without waiting for a reboot.
setup_nftables() {
    log "installing nftables ruleset"
    # firewalld and a hand-managed ruleset will fight. Pick one.
    systemctl disable --now firewalld >/dev/null 2>&1 || true
    systemctl mask firewalld >/dev/null 2>&1 || true

    install -m 0600 "${STATE_DIR}/nftables/gitea.nft" /etc/sysconfig/nftables.conf
    nft -c -f /etc/sysconfig/nftables.conf || die "nftables ruleset failed validation"
    systemctl enable --now nftables
    systemctl reload nftables

    # If this fails, the ruleset flushed something it should not have.
    nft list table inet gitea_filter >/dev/null || die "gitea_filter table missing after reload"
}

# ---------------------------------------------------------------------------
# Artifact Registry credentials for root podman
# ---------------------------------------------------------------------------
install_ar_auth() {
    log "installing Artifact Registry auth refresher"
    cat > /usr/local/bin/gitea-ar-auth <<'EOF'
#!/usr/bin/env bash
# Writes a docker-format auth file for Artifact Registry using the VM service
# account's metadata token.
#
# podman-auto-update.service runs as root with no interactive gcloud session, so
# it needs a credential sitting on disk. This is the #1 reason auto-update
# quietly stops working on GCE.
set -euo pipefail

AR_HOST=$(curl -fsS -H 'Metadata-Flavor: Google' \
    http://169.254.169.254/computeMetadata/v1/instance/attributes/ar-host)
TOKEN=$(curl -fsS -H 'Metadata-Flavor: Google' \
    http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token \
    | jq -r .access_token)

[[ -n "${TOKEN}" && "${TOKEN}" != "null" ]] || { echo "no access token from metadata server" >&2; exit 1; }

AUTH=$(printf 'oauth2accesstoken:%s' "${TOKEN}" | base64 -w0)
umask 077
tmp=$(mktemp /etc/containers/.ar-auth.XXXXXX)
jq -n --arg host "${AR_HOST}" --arg auth "${AUTH}" \
    '{auths: {($host): {auth: $auth}}}' > "${tmp}"
chmod 0600 "${tmp}"
mv "${tmp}" /etc/containers/ar-auth.json
EOF
    chmod 0755 /usr/local/bin/gitea-ar-auth
    /usr/local/bin/gitea-ar-auth || warn "initial AR auth refresh failed"
}

# ---------------------------------------------------------------------------
# Helper scripts
# ---------------------------------------------------------------------------
install_helpers() {
    log "installing helper scripts"

    cat > /usr/local/bin/gitea-backup <<EOF
#!/usr/bin/env bash
# Streams a portable \`gitea dump\` straight to Cloud Storage.
#
# Complements the PD snapshot policy rather than replacing it: a dump restores
# onto any host, a snapshot only restores this disk.
set -euo pipefail
BUCKET="${BACKUP_BUCKET}"
[[ -n "\${BUCKET}" ]] || { echo "no backup bucket configured" >&2; exit 0; }
stamp=\$(date -u +%Y%m%dT%H%M%SZ)
podman exec -u 1000 gitea gitea dump -c /etc/gitea/app.ini -t /tmp -f - \\
    | gcloud storage cp - "gs://\${BUCKET}/dumps/gitea-\${stamp}.zip"
echo "backup complete: gs://\${BUCKET}/dumps/gitea-\${stamp}.zip"
EOF
    chmod 0755 /usr/local/bin/gitea-backup

    cat > /usr/local/bin/gitea-reboot-if-needed <<'EOF'
#!/usr/bin/env bash
# Reboots only when the package layer says a reboot is genuinely required.
#
# `needs-restarting -r` exits 0 for "no reboot needed" and 1 for "reboot
# needed". Anything else -- most likely 127 because the dnf5 plugin is packaged
# differently on this release -- means we do not KNOW, and "do not know" must
# never mean "reboot the Gitea host every Sunday".
set -uo pipefail

dnf needs-restarting -r >/dev/null 2>&1
rc=$?

case "${rc}" in
    0) echo "no reboot required" ; exit 0 ;;
    1) echo "reboot required by pending updates -- rebooting" ; systemctl reboot ;;
    *) echo "needs-restarting returned ${rc} (plugin missing?) -- NOT rebooting" >&2
       echo "install the dnf needs-restarting plugin, or this check is inert" >&2
       exit 0 ;;
esac
EOF
    chmod 0755 /usr/local/bin/gitea-reboot-if-needed
}

# ---------------------------------------------------------------------------
# Config sync + render
# ---------------------------------------------------------------------------
sync_config() {
    log "syncing configuration from gs://${CONFIG_BUCKET}/vm/"
    mkdir -p "${STATE_DIR}"
    gcloud storage rsync --recursive --delete-unmatched-destination-objects \
        "gs://${CONFIG_BUCKET}/vm" "${STATE_DIR}" \
        || die "config sync failed"
}

# Reads a Gitea secret from Secret Manager. If it has no version yet, generates
# one -- but ONLY if a Gitea image is available locally to generate it with.
#
# INTERNAL_TOKEN must be a valid Gitea-issued JWT, so this cannot be a random
# string from Pulumi. Normally scripts/bootstrap.sh has already populated these
# before the first `pulumi up`; this is the safety net.
fetch_or_create_secret() {
    local name="$1" value=""
    if value=$(gcloud secrets versions access latest --secret="${name}" --project="${GCP_PROJECT}" 2>/dev/null); then
        printf '%s' "${value}"
        return 0
    fi

    if ! podman image exists "${IMAGE_GITEA}" 2>/dev/null; then
        return 1
    fi

    local key
    case "${name}" in
        *secret-key)         key=SECRET_KEY ;;
        *internal-token)     key=INTERNAL_TOKEN ;;
        *oauth2-jwt-secret)  key=JWT_SECRET ;;
        *lfs-jwt-secret)     key=LFS_JWT_SECRET ;;
        *) return 1 ;;
    esac

    log "generating missing secret ${name}"
    value=$(podman run --rm "${IMAGE_GITEA}" generate secret "${key}") || return 1
    printf '%s' "${value}" \
        | gcloud secrets versions add "${name}" --project="${GCP_PROJECT}" --data-file=- >/dev/null || return 1
    printf '%s' "${value}"
}

# Renders src -> dst only if the content actually differs, and reports whether
# it changed. Keeps config-sync from restarting healthy services for no reason.
render() {
    local src="$1" dst="$2" owner="$3" mode="$4" vars="$5"
    local tmp
    tmp=$(mktemp)
    envsubst "${vars}" < "${src}" > "${tmp}"
    if [[ -f "${dst}" ]] && cmp -s "${tmp}" "${dst}"; then
        rm -f "${tmp}"
        return 1
    fi
    install -o "${owner%:*}" -g "${owner#*:}" -m "${mode}" "${tmp}" "${dst}"
    rm -f "${tmp}"
    log "rendered ${dst}"
    return 0
}

# Decides whether Caddy can live on the podman bridge or needs the host network.
#
# The googleclouddns ACME plugin authenticates via Application Default
# Credentials, which on GCE means reaching the metadata server at
# 169.254.169.254. If a container on our bridge cannot reach it, DNS-01 issuance
# fails at certificate time -- long after this script has reported success -- so
# the check happens here, up front, and the answer is cached.
probe_caddy_network() {
    local cache=/etc/gitea/caddy-network
    if [[ -f "${cache}" ]]; then
        cat "${cache}"
        return 0
    fi
    if ! podman image exists "${IMAGE_GITEA}" 2>/dev/null; then
        # Cannot probe yet (first boot, before the first image build). Assume the
        # bridge and re-probe on the next config-sync.
        echo bridge
        return 0
    fi

    # The network normally does not exist yet: on a full boot this runs before
    # the quadlet units start. Creating it here with the same arguments quadlet
    # uses keeps the probe honest -- otherwise `podman run --network gitea`
    # fails and the probe wrongly concludes the metadata server is unreachable.
    # stdout is discarded because this function's stdout IS the return value.
    podman network create --ignore \
        --subnet "${PODMAN_SUBNET}" --gateway "${PODMAN_GATEWAY}" gitea >/dev/null 2>&1 || true

    local mode=host
    if podman run --rm --network gitea --entrypoint curl "${IMAGE_GITEA}" \
        -fsS -m 10 -H 'Metadata-Flavor: Google' \
        http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token \
        >/dev/null 2>&1; then
        mode=bridge
    else
        warn "metadata server unreachable from the podman bridge -- Caddy will use the host network"
    fi
    mkdir -p /etc/gitea
    echo "${mode}" > "${cache}"
    echo "${mode}"
}

render_all() {
    log "rendering configuration"
    mkdir -p /etc/gitea /etc/caddy "${RENDER_DIR}"

    local secret_key internal_token oauth2_jwt lfs_jwt
    if ! secret_key=$(fetch_or_create_secret gitea-secret-key) \
       || ! internal_token=$(fetch_or_create_secret gitea-internal-token) \
       || ! oauth2_jwt=$(fetch_or_create_secret gitea-oauth2-jwt-secret) \
       || ! lfs_jwt=$(fetch_or_create_secret gitea-lfs-jwt-secret); then
        # Deliberately NOT a failure: on first boot the image does not exist yet
        # and the secrets may not be populated. Writing an app.ini with empty
        # SECRET_KEY/INTERNAL_TOKEN would be far worse than doing nothing --
        # Gitea would come up with broken sessions and tokens.
        warn "Gitea secrets unavailable -- skipping config render (will retry on the next sync)"
        return 0
    fi

    local caddy_mode caddy_network caddy_publish caddy_sysctl gitea_upstream
    caddy_mode=$(probe_caddy_network)
    if [[ "${caddy_mode}" == "host" ]]; then
        caddy_network="host"
        caddy_publish="# Network=host: ports are bound directly, publishing would be invalid."
        caddy_sysctl="# Network=host: podman rejects net.* sysctls; set on the host instead."
        gitea_upstream="127.0.0.1:3000"
        # Container-local sysctls are unavailable on the host network, so allow
        # unprivileged binds to 80/443 host-wide. Narrower than granting the
        # container CAP_NET_BIND_SERVICE.
        echo 'net.ipv4.ip_unprivileged_port_start = 80' > /etc/sysctl.d/90-gitea-caddy.conf
        sysctl -q -p /etc/sysctl.d/90-gitea-caddy.conf || warn "could not apply unprivileged port sysctl"
    else
        caddy_network="gitea.network"
        caddy_publish=$'PublishPort=80:80\nPublishPort=443:443\nPublishPort=443:443/udp'
        caddy_sysctl="Sysctl=net.ipv4.ip_unprivileged_port_start=0"
        gitea_upstream="gitea:3000"
        rm -f /etc/sysctl.d/90-gitea-caddy.conf
    fi

    # Trust both the bridge CIDR and loopback so this value stays correct in
    # either Caddy networking mode. Rootful podman SNATs host-loopback traffic
    # to the bridge gateway, so the CIDR covers the host-network case too.
    local trusted_proxies="${PODMAN_SUBNET},127.0.0.1/32"

    local changed=0

    export GITEA_SECRET_KEY="${secret_key}" \
           GITEA_INTERNAL_TOKEN="${internal_token}" \
           GITEA_OAUTH2_JWT_SECRET="${oauth2_jwt}" \
           GITEA_LFS_JWT_SECRET="${lfs_jwt}" \
           TRUSTED_PROXIES="${trusted_proxies}" \
           GITEA_UPSTREAM="${gitea_upstream}" \
           CADDY_NETWORK="${caddy_network}" \
           CADDY_PUBLISH_PORTS="${caddy_publish}" \
           CADDY_SYSCTL="${caddy_sysctl}"

    # app.ini is 0400 owned by uid 1000: it holds SECRET_KEY and INTERNAL_TOKEN,
    # and the container runs as that uid and must be able to read it.
    render "${STATE_DIR}/config/app.ini.tmpl" /etc/gitea/app.ini 1000:1000 0400 \
        '${APP_NAME} ${DOMAIN} ${GITEA_SECRET_KEY} ${GITEA_INTERNAL_TOKEN} ${GITEA_OAUTH2_JWT_SECRET} ${GITEA_LFS_JWT_SECRET} ${TRUSTED_PROXIES} ${REQUIRE_SIGNIN_VIEW}' \
        && changed=1

    render "${STATE_DIR}/config/Caddyfile.tmpl" /etc/caddy/Caddyfile root:root 0644 \
        '${DOMAIN} ${ACME_EMAIL} ${GITEA_UPSTREAM} ${WAF_MODE}' \
        && changed=1

    local unit
    for unit in gitea.network gitea.container caddy.container caddy-data.volume caddy-config.volume; do
        render "${STATE_DIR}/quadlets/${unit}" "${RENDER_DIR}/${unit}" root:root 0644 \
            '${IMAGE_GITEA} ${IMAGE_CADDY} ${PODMAN_SUBNET} ${PODMAN_GATEWAY} ${GCP_PROJECT} ${CADDY_NETWORK} ${CADDY_PUBLISH_PORTS} ${CADDY_SYSCTL}' \
            && changed=1
    done

    for unit in "${STATE_DIR}"/systemd/*; do
        [[ -f "${unit}" ]] || continue
        local base; base=$(basename "${unit}")
        if ! cmp -s "${unit}" "/etc/systemd/system/${base}"; then
            install -m 0644 "${unit}" "/etc/systemd/system/${base}"
            log "installed unit ${base}"
            changed=1
        fi
    done

    unset GITEA_SECRET_KEY GITEA_INTERNAL_TOKEN GITEA_OAUTH2_JWT_SECRET GITEA_LFS_JWT_SECRET

    systemctl daemon-reload
    if (( changed )); then
        log "configuration changed -- restarting services"
        systemctl restart gitea.service  || warn "gitea did not restart cleanly"
        systemctl restart caddy.service  || warn "caddy did not restart cleanly"
    else
        log "configuration unchanged"
    fi
}

# ---------------------------------------------------------------------------
# fail2ban
# ---------------------------------------------------------------------------
# Runs on every invocation, not just full boots: the jail's enabled flag is
# derived from WAF_MODE, and that has to be re-applied whenever the mode changes.
setup_fail2ban() {
    command -v fail2ban-server >/dev/null 2>&1 || { warn "fail2ban not installed; skipping"; return 0; }
    log "configuring fail2ban"
    install -m 0644 "${STATE_DIR}/fail2ban/action.d/nft-prerouting.conf" /etc/fail2ban/action.d/
    install -m 0644 "${STATE_DIR}/fail2ban/filter.d/gitea.conf"          /etc/fail2ban/filter.d/
    install -m 0644 "${STATE_DIR}/fail2ban/filter.d/caddy-coraza.conf"   /etc/fail2ban/filter.d/

    # One config value drives both halves: the WAF only blocks in On, and only
    # then is it safe to escalate a WAF verdict into an nftables ban. In
    # DetectionOnly the jail is inert so tuning cannot lock anyone out.
    local coraza_enabled=false
    [[ "${WAF_MODE}" == "On" ]] && coraza_enabled=true
    export CORAZA_JAIL_ENABLED="${coraza_enabled}"
    log "coraza fail2ban jail enabled=${coraza_enabled} (waf-mode=${WAF_MODE})"

    envsubst '${PODMAN_SUBNET} ${CORAZA_JAIL_ENABLED}' \
        < "${STATE_DIR}/fail2ban/jail.d/gitea.local" > /etc/fail2ban/jail.d/gitea.local
    chmod 0644 /etc/fail2ban/jail.d/gitea.local
    systemctl enable --now fail2ban
    systemctl reload fail2ban || systemctl restart fail2ban
}

# ---------------------------------------------------------------------------
# Automatic updates
# ---------------------------------------------------------------------------
setup_auto_updates() {
    log "configuring automatic updates"
    install -m 0644 "${STATE_DIR}/dnf/automatic.conf" /etc/dnf/automatic.conf

    # AlmaLinux 10 ships dnf5, where the unit is dnf5-automatic.timer -- but the
    # package providing it has moved around between releases, so resolve it
    # instead of hardcoding a name that may not exist.
    local timer=""
    for candidate in dnf5-automatic.timer dnf-automatic.timer; do
        if systemctl list-unit-files "${candidate}" >/dev/null 2>&1 \
           && systemctl cat "${candidate}" >/dev/null 2>&1; then
            timer="${candidate}"; break
        fi
    done
    if [[ -z "${timer}" ]]; then
        log "no dnf automatic timer present -- installing provider"
        dnf -y install "$(dnf -q provides '*/dnf5-automatic.timer' 2>/dev/null | awk 'NR==1{print $1}')" \
            || dnf -y install dnf-automatic \
            || warn "could not install a dnf-automatic provider"
        for candidate in dnf5-automatic.timer dnf-automatic.timer; do
            systemctl cat "${candidate}" >/dev/null 2>&1 && { timer="${candidate}"; break; }
        done
    fi
    [[ -n "${timer}" ]] && systemctl enable --now "${timer}" || warn "no dnf automatic timer enabled"

    systemctl enable --now podman-auto-update.timer
}

enable_units() {
    log "enabling units"
    systemctl daemon-reload
    systemctl enable --now gitea-ar-auth.timer
    systemctl enable --now gitea-backup.timer
    systemctl enable --now gitea-reboot-window.timer
    # Quadlet-generated units are not "enabled" in the usual sense -- the
    # [Install] section is honoured by the generator at daemon-reload time.
    systemctl start gitea.service || warn "gitea not started yet (expected before the first image build)"
    systemctl start caddy.service || warn "caddy not started yet (expected before the first image build)"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    log "starting (mode=${MODE})"

    if [[ "${MODE}" == "full" ]]; then
        install_packages
        setup_data_disk
        setup_swap
        configure_podman
        install_helpers
    fi

    sync_config

    # Make the synced copy the canonical one, so gitea-config-sync.service always
    # runs the version that matches the config in the bucket.
    #
    # Under --sync-only this file IS the script bash is currently reading. GNU
    # install truncates in place and bash reads scripts incrementally, so a
    # naive copy can rewrite the interpreter's input mid-execution -- exactly in
    # the case this mechanism exists for (a vm/bootstrap.sh change). Skip when
    # identical, and otherwise replace via atomic rename onto a fresh inode so
    # the running process keeps reading the old one.
    if ! cmp -s "${STATE_DIR}/bootstrap.sh" /usr/local/sbin/gitea-bootstrap; then
        install -m 0755 "${STATE_DIR}/bootstrap.sh" /usr/local/sbin/.gitea-bootstrap.new
        mv -f /usr/local/sbin/.gitea-bootstrap.new /usr/local/sbin/gitea-bootstrap
        log "updated /usr/local/sbin/gitea-bootstrap"
    fi

    if [[ "${MODE}" == "full" ]]; then
        install_ar_auth
    fi

    # setup_nftables and setup_fail2ban run in BOTH modes, deliberately.
    #
    # They apply configuration that lives in the vm/ tree, so gating them on a
    # full boot would mean a pushed change never takes effect until the next
    # reboot. The fail2ban case is the dangerous one: flipping gitea:wafMode to
    # On re-renders the Caddyfile through render_all and Caddy starts issuing
    # 403s, but the jail that acts on them would stay enabled=false -- a WAF
    # that blocks and a ban that never happens, with nothing in the logs to say
    # so. Both functions are idempotent and self-validating (`nft -c` before
    # load, `command -v fail2ban-server` before touching fail2ban).
    setup_nftables
    setup_fail2ban

    render_all

    if [[ "${MODE}" == "full" ]]; then
        setup_auto_updates
        enable_units
    fi

    log "done"
}

main "$@"
