#!/usr/bin/env bats
# start.sh, the DinD runner container's command, against a scan directory in a
# temp dir and a stub s6-svscan that records how it was called.
# Run: bats infra/pkg/runner/bats (make check does).

SCRIPT="$BATS_TEST_DIRNAME/../start.sh"

setup() {
  export START_SCAN_DIR="$BATS_TEST_TMPDIR/s6"
  export START_PRUNE_SCRIPT=/etc/gitea-runner/image-prune.sh
  mkdir -p "$START_SCAN_DIR/docker" "$BATS_TEST_TMPDIR/bin"
  # A stub s6-svscan: records its arguments instead of supervising anything.
  printf '#!/bin/sh\necho "$@" > "%s/svscan-args"\n' "$BATS_TEST_TMPDIR" >"$BATS_TEST_TMPDIR/bin/s6-svscan"
  chmod 0755 "$BATS_TEST_TMPDIR/bin/s6-svscan"
  export PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

@test "it installs an image-prune service that runs the prune script with bash" {
  run sh "$SCRIPT"
  [ "$status" -eq 0 ]
  [ -x "$START_SCAN_DIR/image-prune/run" ]
  [ "$(tail -n 1 "$START_SCAN_DIR/image-prune/run")" = "exec bash /etc/gitea-runner/image-prune.sh" ]
  [ "$(head -n 1 "$START_SCAN_DIR/image-prune/run")" = "#!/bin/sh" ]
}

@test "the service has no finish script, so a prune failure can't stop the runner" {
  run sh "$SCRIPT"
  [ "$status" -eq 0 ]
  [ ! -e "$START_SCAN_DIR/image-prune/finish" ]
}

@test "it hands over to s6-svscan on the scan directory" {
  run sh "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(cat "$BATS_TEST_TMPDIR/svscan-args")" = "$START_SCAN_DIR" ]
}

@test "a service directory left from an earlier start is fine" {
  mkdir -p "$START_SCAN_DIR/image-prune"
  echo stale >"$START_SCAN_DIR/image-prune/run"
  run sh "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(tail -n 1 "$START_SCAN_DIR/image-prune/run")" = "exec bash /etc/gitea-runner/image-prune.sh" ]
}

@test "it refuses an image whose scan directory has no docker service" {
  rmdir "$START_SCAN_DIR/docker"
  run sh "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"the runner image changed"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/svscan-args" ]
}

@test "it refuses an image without s6-svscan" {
  rm "$BATS_TEST_TMPDIR/bin/s6-svscan"
  PATH="$BATS_TEST_TMPDIR/bin:/usr/bin:/bin" run sh "$SCRIPT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"the runner image changed"* ]]
}

@test "without overrides it uses the prod paths" {
  run rg -n -F -e 'scan_dir="${START_SCAN_DIR:-/etc/s6}"' -e 'prune_script="${START_PRUNE_SCRIPT:-/etc/gitea-runner/image-prune.sh}"' "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 2 ]
}
