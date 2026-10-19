#!/usr/bin/env bash
#
# VM-side steps for scripts/ops/gitea_ops.py. Runs as root ON gitea-vm.
#
# gitea_ops.py ships this file over IAP SSH for every call and runs one step;
# it is never installed on the VM. Installing it would mean editing
# vm/bootstrap.sh, and the whole of that script is the instance's
# metadataStartupScript -- any edit replaces the VM.
#
# Each step is one small action. The orchestrator owns every decision and the
# order they happen in, so it can run from a workstation or from Cloud Build
# and roll back from wherever it got to.
#
# Usage: vm-steps.sh <step> [args...]

set -euo pipefail

readonly AUTHFILE=/etc/containers/ar-auth.json
readonly DATA=/var/lib/gitea
readonly APP_INI=/etc/gitea/app.ini
# The device name gitea_ops.py attaches a snapshot-derived disk under.
readonly RESTORE_DEV=/dev/disk/by-id/google-gitea-restore
readonly RESTORE_MNT=/mnt/gitea-restore
readonly TEST_CTR=gitea-restore-test
# The two timers that must not fire mid-release: auto-update would race the
# rollout, and the reboot window would reboot with a restore disk attached.
readonly TIMERS=(podman-auto-update.timer gitea-reboot-window.timer)

log() { echo "[vm-steps] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# The credential file is refreshed by a timer; a long-idle box can hold a
# stale one, and a pull that fails on auth looks like a missing image.
refresh_auth() { systemctl start gitea-ar-auth.service; }

# A private copy of app.ini for one-shot containers. Mounting the real file
# with :Z would relabel it with THAT container's private MCS category, and the
# running gitea container would lose read access to its own config.
private_app_ini() {
    local dir
    dir=$(mktemp -d /run/gitea-ops.XXXXXX)
    install -o 1000 -g 1000 -m 0400 "${APP_INI}" "${dir}/app.ini"
    echo "${dir}/app.ini"
}

# Runs a throwaway gitea container against a data directory: no network, no
# published ports, a private app.ini. Used for migrations and restore tests.
gitea_oneshot() {
    local img="$1" data="$2"; shift 2
    local ini
    ini=$(private_app_ini)
    podman run --rm --network none --user 1000:1000 \
        -v "${data}:/var/lib/gitea" \
        -v "${ini}:/etc/gitea/app.ini:ro,Z" \
        -e GITEA_WORK_DIR=/var/lib/gitea \
        "${img}" "$@"
    rm -rf "$(dirname "${ini}")"
}

require_stopped() {
    if systemctl is-active --quiet gitea.service; then
        die "gitea.service is running; stop it first"
    fi
}

mount_restore() {
    # udev publishes the by-id link a moment after attach-disk returns.
    local _
    for _ in $(seq 1 30); do
        [[ -e "${RESTORE_DEV}" ]] && break
        sleep 1
    done
    [[ -e "${RESTORE_DEV}" ]] || die "no restore disk attached as device name gitea-restore"
    mkdir -p "${RESTORE_MNT}"
    mountpoint -q "${RESTORE_MNT}" && umount "${RESTORE_MNT}"
    # nouuid: the copy carries the SAME XFS UUID as the live /var/lib/gitea,
    # and XFS refuses to mount a second filesystem with a UUID it already has.
    # It is also why the disk must be detached before any reboot: fstab mounts
    # /var/lib/gitea by UUID and could pick the copy.
    mount -o nouuid "${RESTORE_DEV}" "${RESTORE_MNT}"
}

# inventory DATA_DIR TOOL_IMAGE: what a Gitea data tree holds, as TSV lines
#
#   integrity  <sqlite integrity_check verdict>
#   user       <lower_name> <type> <created_unix>
#   repo       <owner>/<name> <created_unix> <updated_unix> <sha256 of its refs | missing>
#
# This is what a restore is judged on: identities and ref hashes, not counts,
# so a copy holding 2 of 40 repos cannot pass. Read-only: sqlite3 -readonly
# never checkpoints a live WAL, and git only lists refs. Runs as uid 1000 so
# any -shm file sqlite3 creates stays owned by the user Gitea runs as.
inventory() {
    local data="$1" tool="$2"
    podman run --rm --network none --user 1000:1000 --security-opt label=disable \
        -e HOME=/tmp -v "${data}:/d" --entrypoint /bin/sh "${tool}" -euc '
        T=$(printf "\t")
        db=/d/data/gitea.db
        [ -f "$db" ] || { echo "no $db" >&2; exit 1; }
        printf "integrity\t%s\n" "$(sqlite3 -readonly "$db" "PRAGMA integrity_check;" | head -1)"
        sqlite3 -readonly -separator "$T" "$db" "SELECT lower_name, type, created_unix FROM user;" |
        while IFS="$T" read -r name type created; do
            printf "user\t%s\t%s\t%s\n" "$name" "$type" "$created"
        done
        sqlite3 -readonly -separator "$T" "$db" \
            "SELECT lower(owner_name), lower_name, created_unix, updated_unix FROM repository;" |
        while IFS="$T" read -r owner name created updated; do
            dir="/d/data/gitea-repositories/$owner/$name.git"
            if [ -d "$dir" ]; then
                refs=$(git --git-dir="$dir" for-each-ref --format="%(objectname) %(refname)" | sha256sum | cut -d" " -f1)
            else
                refs=missing
            fi
            printf "repo\t%s/%s\t%s\t%s\t%s\n" "$owner" "$name" "$created" "$updated" "$refs"
        done' | sort
}

# File count of a data tree, leaving out moved-aside .pre-restore-* trees.
count_files() {
    (cd "$1" && find . -path './.pre-restore-*' -prune -o -print | wc -l)
}

# --- steps ------------------------------------------------------------------

# inventory TOOL_IMAGE [DATA_DIR]: the inventory of the live tree (default).
step_inventory() { inventory "${2:-${DATA}}" "$1"; }

# pull IMAGE...: pull by reference, print each local digest on stdout.
step_pull() {
    refresh_auth
    local img
    for img in "$@"; do
        podman pull --authfile "${AUTHFILE}" "${img}" >&2
        podman image inspect "${img}" --format '{{.Digest}}'
    done
}

# pause-timers [DEADMAN]: stop the timers, and arm a dead man's switch.
#
# The engine resumes the timers itself, but only if it is still alive to. If
# it dies mid-release (its pod, its node, a laptop), the switch re-enables
# them after DEADMAN (default 3h, longer than the engine Job's 2h deadline)
# and logs at crit. It does NOT touch gitea.service: whether starting it is
# safe depends on how far a migration got, which is a decision for a person.
# And with a restore disk still attached it leaves the timers paused too,
# because the reboot window would then boot onto a copy (fstab mounts by UUID).
step_pause_timers() {
    systemctl stop "${TIMERS[@]}"
    disarm_deadman
    systemd-run --quiet --unit=gitea-ops-deadman --on-active="${1:-3h}" \
        --timer-property=AccuracySec=1min \
        --description="Re-enable Gitea's timers if the release engine died" \
        /bin/bash -c '
            if [ -e /dev/disk/by-id/google-gitea-restore ]; then
                logger -p user.crit -t gitea-ops "DEADMAN: the release engine never came back and a restore disk is attached; timers left paused"
            else
                systemctl start podman-auto-update.timer gitea-reboot-window.timer
                logger -p user.crit -t gitea-ops "DEADMAN: the release engine never came back; timers re-enabled; gitea.service is $(systemctl is-active gitea.service)"
            fi'
}
step_resume_timers() { systemctl start "${TIMERS[@]}"; disarm_deadman; }
disarm_deadman() {
    systemctl stop gitea-ops-deadman.timer gitea-ops-deadman.service 2>/dev/null || true
    systemctl reset-failed gitea-ops-deadman.service 2>/dev/null || true
}

# stop UNIT / start UNIT. start blocks until the quadlet's Notify=healthy
# fires or TimeoutStartSec runs out, so its exit status is the health verdict.
step_stop()  { systemctl stop "$1"; sync; }
step_start() { systemctl start "$1"; }

# restart-if-not DIGEST UNIT CONTAINER: restart only when the container is not
# already running DIGEST (after a :prod tag move and a pull).
step_restart_if_not() {
    local want="$1" unit="$2" ctr="$3" have
    have=$(podman inspect "${ctr}" --format '{{.ImageDigest}}' 2>/dev/null || true)
    if [[ "${have}" == "${want}" ]]; then
        log "${ctr} already runs ${want}"
        return 0
    fi
    systemctl restart "${unit}"
}

# migrate IMAGE: the explicit schema migration, run as a one-shot BEFORE the
# unit starts. Inside the unit, a long migration would be killed by
# TimeoutStartSec and re-run on a half-migrated database.
step_migrate() {
    local img="$1"
    require_stopped
    local t0=${SECONDS}
    gitea_oneshot "${img}" "${DATA}" migrate --config /etc/gitea/app.ini
    log "migrate finished in $(( SECONDS - t0 ))s"
    # Report-only: doctor flags fixable oddities that are no reason to abort.
    gitea_oneshot "${img}" "${DATA}" doctor check --config /etc/gitea/app.ini >&2 \
        || log "WARNING: doctor check reported problems (not fatal)"
}

step_auto_update() {
    refresh_auth
    systemctl start podman-auto-update.service
    journalctl -u podman-auto-update -n 20 --no-pager -o cat >&2 || true
}

# running-digest CONTAINER: the image digest the container is actually running.
step_running_digest() { podman inspect "$1" --format '{{.ImageDigest}}'; }

# restarts UNIT: systemd's restart counter, compared before/after a soak.
step_restarts() { systemctl show "$1" -p NRestarts --value; }

# journal-errors UNIT SINCE: fatal and panic lines since SINCE (a UTC time).
step_journal_errors() {
    journalctl -u "$1" --since "$2" --no-pager -o cat 2>/dev/null \
        | grep -cE '\[F\]|panic:' || true
}

# dump GS_URL: a portable gitea dump streamed straight to Cloud Storage, as
# the VM's own service account. Same pipeline as /usr/local/bin/gitea-backup,
# but the caller picks the object name so it can check the result.
step_dump() {
    local url="$1"
    podman exec -u 1000 gitea gitea dump -c /etc/gitea/app.ini -t /tmp -f - \
        | gcloud storage cp - "${url}"
}

# config-tarball GS_URL: everything rendered or installed onto the host, so a
# config regression can be diffed against the last good one. It contains the
# rendered app.ini, so it never leaves GCP: it streams from here to the bucket.
step_config_tarball() {
    local url="$1"
    tar -C / --ignore-failed-read -czf - \
        etc/gitea etc/caddy \
        etc/containers/systemd etc/containers/containers.conf.d \
        etc/fail2ban/jail.d etc/fail2ban/filter.d etc/fail2ban/action.d \
        etc/sysconfig/nftables.conf etc/fstab \
        etc/systemd/system/gitea-ar-auth.service etc/systemd/system/gitea-ar-auth.timer \
        etc/systemd/system/gitea-backup.service etc/systemd/system/gitea-backup.timer \
        etc/systemd/system/gitea-config-sync.service \
        etc/systemd/system/gitea-reboot-window.service etc/systemd/system/gitea-reboot-window.timer \
        usr/local/sbin/gitea-bootstrap opt/gitea-config \
        | gcloud storage cp - "${url}"
}

# marker NAME: drop a file in the live data directory. The data-restore drill
# writes one AFTER its snapshot; a restore that really happened removes it.
step_marker()       { install -o 1000 -g 1000 -m 0644 /dev/null "${DATA}/.drill-$1"; }
step_marker_check() { [[ -e "${DATA}/.drill-$1" ]] && echo present || echo absent; }

# restore-sync TOOL_IMAGE: replace /var/lib/gitea with the attached snapshot copy.
#
# The current contents are moved aside (same filesystem, so instant) rather
# than deleted: they are the evidence of what went wrong, and if the copy fails
# halfway, nothing is lost and it can simply be re-run. Delete
# /var/lib/gitea/.pre-restore-* by hand once things are understood.
#
# The host has no rsync, unzip or sqlite3; our own gitea image carries all
# three for exactly this, so TOOL_IMAGE is any image this repo built. It runs
# as root (ownership must survive), with no network, and with SELinux
# separation off for this one container so it can read the copy and write the
# live tree without relabelling either -- restorecon re-asserts the labels.
step_restore_sync() {
    local tool="$1"
    require_stopped
    mount_restore
    trap 'umount "${RESTORE_MNT}" 2>/dev/null || true' EXIT
    [[ -f "${RESTORE_MNT}/data/gitea.db" ]] \
        || die "${RESTORE_MNT} has no data/gitea.db -- not a gitea-data snapshot?"

    local keep
    keep="${DATA}/.pre-restore-$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir "${keep}"
    find "${DATA}" -mindepth 1 -maxdepth 1 ! -name '.pre-restore-*' -exec mv -t "${keep}" {} +
    podman run --rm --network none --user 0:0 --security-opt label=disable \
        -v "${RESTORE_MNT}:/src:ro" -v "${DATA}:/dst" \
        --entrypoint rsync "${tool}" \
        -aH --numeric-ids --exclude='/.pre-restore-*' /src/ /dst/
    restorecon -RF "${DATA}" || log "WARNING: restorecon failed"
    sync

    # Guard: the live tree must now hold exactly what the snapshot held --
    # same files, same users, same repos with the same refs -- or Gitea is
    # not started on it. A partial copy fails here, not in production.
    # Captured, not diffed through <(...): a failed process substitution is
    # invisible, and two empty inventories would "match".
    local want have inv_src inv_dst
    want=$(count_files "${RESTORE_MNT}"); have=$(count_files "${DATA}")
    [[ "${want}" == "${have}" ]] \
        || die "restore incomplete: ${have} files in ${DATA}, ${want} in the snapshot (old contents kept in ${keep})"
    inv_src=$(inventory "${RESTORE_MNT}" "${tool}") || die "could not inventory the snapshot copy"
    inv_dst=$(inventory "${DATA}" "${tool}") || die "could not inventory ${DATA}"
    grep -qx $'integrity\tok' <<<"${inv_src}" || die "the snapshot's database fails sqlite integrity_check"
    if [[ "${inv_src}" != "${inv_dst}" ]]; then
        diff <(echo "${inv_src}") <(echo "${inv_dst}") >&2 || true
        die "restored tree does not match the snapshot (old contents kept in ${keep})"
    fi
    log "restored ${DATA} (${have} files, inventory matches the snapshot); the replaced contents are in ${keep}"
}

# Starts the isolated test instance on DATA_DIR. --network none: a restored
# copy must never push mirrors, fire webhooks or talk to runners. Prints the
# inventory taken BEFORE the boot (the boot may migrate the schema), then
# "boot<TAB>ok". gitea_ops.py compares the inventory with prod's.
restore_test_instance() {
    local img="$1" data="$2" ini
    inventory "${data}" "${img}"
    podman rm -f "${TEST_CTR}" >/dev/null 2>&1 || true
    ini=$(private_app_ini)
    podman run -d --name "${TEST_CTR}" --network none --user 1000:1000 \
        -v "${data}:/var/lib/gitea" \
        -v "${ini}:/etc/gitea/app.ini:ro,Z" \
        -e GITEA_WORK_DIR=/var/lib/gitea \
        "${img}" >/dev/null
    local ok=1 _
    for _ in $(seq 1 60); do
        if podman exec "${TEST_CTR}" curl -fsS http://127.0.0.1:3000/api/healthz >/dev/null 2>&1; then
            ok=0; break
        fi
        sleep 3
    done
    if [[ ${ok} -ne 0 ]]; then
        podman logs --tail 30 "${TEST_CTR}" >&2 || true
        podman rm -f "${TEST_CTR}" >/dev/null 2>&1 || true
        rm -rf "$(dirname "${ini}")"
        die "restored instance never became healthy"
    fi
    podman rm -f "${TEST_CTR}" >/dev/null
    rm -rf "$(dirname "${ini}")"
    printf 'boot\tok\n'
}

# restore-test-snapshot IMAGE: boot the attached snapshot copy in isolation.
step_restore_test_snapshot() {
    local img="$1"
    mount_restore
    trap 'podman rm -f "${TEST_CTR}" >/dev/null 2>&1 || true; umount "${RESTORE_MNT}" 2>/dev/null || true' EXIT
    restore_test_instance "${img}" "${RESTORE_MNT}"
}

# restore-test-dump GS_URL IMAGE: rebuild an instance from a gitea dump onto
# the attached scratch disk, per Gitea's documented restore, then boot it in
# isolation. The VM's service account can read a dump by exact name only.
#
# The database comes from gitea-db.sql, not from the data/gitea.db the dump
# also carries: that file was copied while Gitea was writing to it.
step_restore_test_dump() {
    local url="$1" img="$2" root
    mount_restore
    trap 'podman rm -f "${TEST_CTR}" >/dev/null 2>&1 || true; umount "${RESTORE_MNT}" 2>/dev/null || true' EXIT
    root="${RESTORE_MNT}/.dumptest"
    rm -rf "${root}"
    mkdir -p "${root}"
    gcloud storage cp "${url}" "${root}/dump.zip" >&2
    podman run --rm --network none --user 0:0 --security-opt label=disable \
        -v "${root}:/w" --entrypoint /bin/sh "${img}" -euc '
        mkdir -p /w/unpacked /w/root/data/gitea-repositories /w/root/log
        cd /w/unpacked && unzip -q ../dump.zip
        cp -a data/. /w/root/data/
        rm -f /w/root/data/gitea.db /w/root/data/gitea.db-wal /w/root/data/gitea.db-shm
        if [ -d repos ]; then cp -a repos/. /w/root/data/gitea-repositories/; fi
        if [ -d custom ]; then cp -a custom /w/root/; fi
        sqlite3 /w/root/data/gitea.db < gitea-db.sql
        chown -R 1000:1000 /w/root
    '
    restore_test_instance "${img}" "${root}/root"
    rm -rf "${root}"
}

main() {
    local step="${1:-}"
    [[ -n "${step}" ]] || die "usage: $0 <step> [args...]"
    shift
    local fn="step_${step//-/_}"
    declare -F "${fn}" >/dev/null || die "unknown step: ${step}"
    "${fn}" "$@"
}

main "$@"
