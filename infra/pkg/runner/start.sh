#!/bin/sh
# The DinD runner container's command (infra/pkg/runner, statefulSet). It adds
# an image-prune service to the image's s6 scan directory, then runs the image's
# own entrypoint, `s6-svscan /etc/s6`, which starts dockerd and the runner as
# before, and now image-prune.sh beside them.
#
# s6-svscan reads its scan directory when it starts, so the service has to be
# there first. It gets no finish script on purpose: the image's two services
# have one that tears down the whole container when their service exits, and a
# prune that fails must not take the runner down. Without one, s6 restarts it.
#
# If a new runner image stops using s6 at /etc/s6, this exits, the container
# never gets ready, and the deploy fails instead of the prune silently
# stopping (docs/runners.md, "Upgrade the runner").

#
# START_SCAN_DIR and START_PRUNE_SCRIPT exist for the tests
# (infra/pkg/runner/bats/start.bats); the pod sets neither.

set -eu

scan_dir="${START_SCAN_DIR:-/etc/s6}"
service_dir="$scan_dir/image-prune"
prune_script="${START_PRUNE_SCRIPT:-/etc/gitea-runner/image-prune.sh}"

if [ ! -d "$scan_dir/docker" ] || ! command -v s6-svscan >/dev/null 2>&1; then
  echo "start.sh: no s6 scan directory with a docker service at $scan_dir; the runner image changed" >&2
  exit 1
fi

mkdir -p "$service_dir"
printf '#!/bin/sh\nexec bash %s\n' "$prune_script" >"$service_dir/run"
chmod 0755 "$service_dir/run"

exec s6-svscan "$scan_dir"
