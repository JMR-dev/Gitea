#!/usr/bin/env bats
# image-prune.sh, one pass at a time (IMAGE_PRUNE_ONCE=1), against a fake
# docker and df (fakes/) that serve state from $FAKE and record what was asked.
# Run: bats infra/pkg/runner/bats (make check does).

SCRIPT="$BATS_TEST_DIRNAME/../image-prune.sh"
LINE=4194304

setup() {
  export FAKE="$BATS_TEST_TMPDIR/fake"
  mkdir -p "$FAKE"
  export PATH="$BATS_TEST_DIRNAME/fakes:$PATH"
  export IMAGE_PRUNE_DATA_ROOT=/fake/data
  export IMAGE_PRUNE_MIN_FREE_KIB="$LINE"
  export IMAGE_PRUNE_KEEP=docker.io/gitea/runner-images:ubuntu-latest
  export IMAGE_PRUNE_STATE="$BATS_TEST_TMPDIR/state"
  export IMAGE_PRUNE_ONCE=1
  echo $((LINE + 1)) >"$FAKE/free"
}

# Three images, tagged a day apart: old, mid, new. Their IDs sort the other way
# round, so only the tag times can put them in this order.
OLD=sha256:cccc3333cccc3333
MID=sha256:bbbb2222bbbb2222
NEW=sha256:aaaa1111aaaa1111
three_images() {
  cat >"$FAKE/images" <<EOF
$OLD 2026-11-01T10:00:00.5Z 100000000 old:1
$MID 2026-11-02T10:00:00.5Z 100000000 mid:1
$NEW 2026-11-03T10:00:00.5Z 100000000 new:1
EOF
}

below_line() {
  echo "$LINE" >"$FAKE/free"
}

removed() {
  cat "$FAKE/rm.log" 2>/dev/null | tr '\n' ' '
}

@test "it refuses to start without each setting Go gives it" {
  for name in IMAGE_PRUNE_DATA_ROOT IMAGE_PRUNE_MIN_FREE_KIB IMAGE_PRUNE_KEEP IMAGE_PRUNE_STATE; do
    run env -u "$name" bash "$SCRIPT"
    [ "$status" -ne 0 ]
    [[ "$output" == *"$name"* ]]
  done
}

@test "above the line it removes nothing" {
  three_images
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(removed)" = "" ]
  [ ! -e "$FAKE/builder.log" ]
}

@test "at the line it removes the least recently used, and stops once above it" {
  three_images
  below_line
  echo 1 >"$FAKE/free_per_rm"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(removed)" = "old:1 " ]
  [ ! -e "$FAKE/builder.log" ]
}

@test "below the line it keeps going in LRU order, then prunes build cache, then says so" {
  three_images
  below_line
  run bash "$SCRIPT"
  [ "$(removed)" = "old:1 mid:1 new:1 " ]
  [ "$(cat "$FAKE/builder.log")" = "$(printf -- '-af --filter until=24h\n-af')" ]
  [[ "$output" == *"still below line"* ]]
}

@test "build cache stops after unused-for-a-day when that's enough" {
  below_line
  echo 10 >"$FAKE/free_per_cache"
  run bash "$SCRIPT"
  [ "$(cat "$FAKE/builder.log")" = "-af --filter until=24h" ]
}

@test "an image a container used recently goes after ones tagged later" {
  three_images
  below_line
  echo "$OLD $(date +%s)" >"$IMAGE_PRUNE_STATE"
  run bash "$SCRIPT"
  [ "$(removed)" = "mid:1 new:1 old:1 " ]
}

@test "an image any container uses, even a stopped one, is kept" {
  three_images
  below_line
  echo "$MID false 2026-11-02T11:00:00Z" >"$FAKE/containers"
  run bash "$SCRIPT"
  [ "$(removed)" = "old:1 new:1 " ]
  [[ "$output" == *"keeping mid:1 "*": used by a container"* ]]
}

@test "the job image is kept" {
  three_images
  below_line
  echo "$OLD" >"$FAKE/keep_id"
  run bash "$SCRIPT"
  [ "$(removed)" = "mid:1 new:1 " ]
  [[ "$output" == *"keeping old:1 "*": the job image"* ]]
}

@test "while a job runs, anything touched since it started is kept" {
  three_images
  echo "sha256:dddd4444dddd4444 2026-11-02T12:00:00.5Z 1 job:1" >>"$FAKE/images"
  below_line
  # The job started between mid's tag and new's.
  echo "sha256:dddd4444dddd4444 true 2026-11-02T12:00:00.123Z" >"$FAKE/containers"
  run bash "$SCRIPT"
  [ "$(removed)" = "old:1 mid:1 " ]
  [[ "$output" == *"keeping new:1 "*": touched by the running job"* ]]
  [[ "$output" == *"keeping job:1 "*": used by a container"* ]]
}

@test "an image goes by all its names at once, a dangling one by its ID" {
  cat >"$FAKE/images" <<'EOF'
sha256:bbbb2222bbbb2222 2026-11-01T10:00:00Z 1 a:1,a:prod,a@sha256:abc
sha256:aaaa1111aaaa1111 2026-11-02T10:00:00Z 1
EOF
  below_line
  run bash "$SCRIPT"
  [ "$(removed)" = "a:1 a:prod a@sha256:abc sha256:aaaa1111aaaa1111 " ]
}

@test "a removal docker refuses is skipped and the next one tried" {
  three_images
  below_line
  echo old:1 >"$FAKE/rm_fail"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(removed)" = "mid:1 new:1 " ]
  [[ "$output" == *"could not remove old:1 "* ]]
}

@test "a dry run lists the order and removes, prunes and saves nothing" {
  three_images
  # new:1 in use by a stopped container, so no running job protects the others.
  echo "$NEW false 2026-11-01T00:00:00Z" >"$FAKE/containers"
  IMAGE_PRUNE_DRY_RUN=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$(removed)" = "" ]
  [ ! -e "$FAKE/builder.log" ]
  [ ! -e "$IMAGE_PRUNE_STATE" ]
  [[ "$output" == *"would remove old:1 "*"would remove mid:1 "* ]]
  [[ "$output" == *"would prune build cache"* ]]
}

@test "the state records when running containers used an image, and forgets gone images" {
  three_images
  echo "sha256:gone0000gone0000 12345" >"$IMAGE_PRUNE_STATE"
  echo "$MID true 2026-11-02T11:00:00Z" >"$FAKE/containers"
  before="$(date +%s)"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  read -r id seen <"$IMAGE_PRUNE_STATE"
  [ "$id" = "$MID" ]
  [ "$seen" -ge "$before" ]
  [ "$(wc -l <"$IMAGE_PRUNE_STATE")" -eq 1 ]
}

@test "a docker that doesn't answer is logged and retried, not fatal" {
  echo ps-fails >"$FAKE/mode"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"docker did not answer"* ]]
}

@test "ERROR when image inspect can't be read for every listed image" {
  three_images
  echo silent-inspect >"$FAKE/mode"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"image-prune: ERROR: could read 0 of 3 images"* ]]
}

@test "ERROR when LastTagTime doesn't parse, and pruning still goes on" {
  cat >"$FAKE/images" <<'EOF'
sha256:aaaa1111aaaa1111 not-a-date 1 old:1
sha256:bbbb2222bbbb2222 2026-11-02T10:00:00Z 1 mid:1
EOF
  below_line
  run bash "$SCRIPT"
  [[ "$output" == *"image-prune: ERROR: 1 images have a LastTagTime it can't read"* ]]
  [ "$(removed)" = "mid:1 old:1 " ]
}

@test "ERROR when a running container's StartedAt doesn't parse, and everything is kept" {
  three_images
  below_line
  echo "sha256:dddd4444dddd4444 true not-a-date" >"$FAKE/containers"
  run bash "$SCRIPT"
  [[ "$output" == *"image-prune: ERROR: 1 running containers have a StartedAt it can't read"* ]]
  [ "$(removed)" = "" ]
  [[ "$output" == *"keeping old:1 "*": touched by the running job"* ]]
}

@test "a never-tagged image (LastTagTime year 1) is no error and goes last" {
  cat >"$FAKE/images" <<'EOF'
sha256:aaaa1111aaaa1111 0001-01-01T00:00:00Z 1 untagged:1
sha256:bbbb2222bbbb2222 2026-11-02T10:00:00Z 1 mid:1
EOF
  below_line
  run bash "$SCRIPT"
  [[ "$output" != *"ERROR"* ]]
  [ "$(removed)" = "mid:1 untagged:1 " ]
}

@test "a pass with nothing wrong logs no ERROR" {
  three_images
  below_line
  run bash "$SCRIPT"
  [[ "$output" != *"ERROR"* ]]
}
