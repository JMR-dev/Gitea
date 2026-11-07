#!/usr/bin/env bats
# bootstrap.sh's Gitea secrets when it has no podman or docker to generate
# them with, against a fake gcloud (fakes/) and a PATH holding only the tools
# bootstrap.sh needs, so neither container runtime is found.
# Run: bats scripts/bats (make check does).

SCRIPT="$BATS_TEST_DIRNAME/../bootstrap.sh"
GITEA_SECRETS="gitea-secret-key gitea-internal-token gitea-oauth2-jwt-secret gitea-lfs-jwt-secret"

setup() {
  export FAKE="$BATS_TEST_TMPDIR/fake"
  mkdir -p "$FAKE"
  BIN="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$BIN"
  local tool
  for tool in bash cat grep seq sleep tr; do
    ln -s "$(command -v "$tool")" "$BIN/$tool"
  done
  ln -s "$BATS_TEST_DIRNAME/fakes/gcloud" "$BIN/gcloud"
  # A new project: Gitea's secrets exist but have no version yet.
  printf '%s\n' $GITEA_SECRETS >"$FAKE/no_version"
}

@test "without podman or docker it says each Gitea secret is missing, and that deploys fail until it's re-run" {
  PATH="$BIN" run "$BIN/bash" "$SCRIPT" p us-east1
  [ "$status" -eq 0 ]
  for name in $GITEA_SECRETS; do
    [[ "$output" == *"cannot generate $name."* ]]
  done
  # The VM never makes them: the deploy fails at app.ini, it doesn't skip it.
  [[ "$output" == *"every deploy fails at app.ini"* ]]
  [[ "$output" != *"skip rendering"* ]]
  [ ! -e "$FAKE/added.log" ]
}
