#!/usr/bin/env bats
# gitea-render-secrets app-ini: Gitea's four secrets, read from Secret Manager,
# and when one is missing. Against a fake gcloud, podman and chown (fakes/)
# that serve state from $FAKE and record what was asked, with the files it
# writes under GITEA_RENDER_ETC, a temp dir, instead of /etc.
# Run: bats vm/ansible/roles/app/files/bats (make check does).

SCRIPT="$BATS_TEST_DIRNAME/../gitea-render-secrets"
IMAGE=registry.example/gitea/gitea:1
SUFFIXES="secret-key internal-token oauth2-jwt-secret lfs-jwt-secret"

setup() {
  export FAKE="$BATS_TEST_TMPDIR/fake"
  mkdir -p "$FAKE/secrets"
  export PATH="$BATS_TEST_DIRNAME/fakes:$PATH"
  export GITEA_RENDER_ETC="$BATS_TEST_TMPDIR/etc"
  mkdir -p "$GITEA_RENDER_ETC/gitea"
  APP_INI="$GITEA_RENDER_ETC/gitea/app.ini"
  # app.ini as the app role hands it over: the secrets still placeholders.
  export GITEA_RENDER_INPUT='[security]
SECRET_KEY = ${GITEA_SECRET_KEY}
INTERNAL_TOKEN = ${GITEA_INTERNAL_TOKEN}
[oauth2]
JWT_SECRET = ${GITEA_OAUTH2_JWT_SECRET}
[server]
LFS_JWT_SECRET = ${GITEA_LFS_JWT_SECRET}
'
  # The Gitea image is on the VM: the app role pulls it before app.ini.
  echo "$IMAGE" >"$FAKE/image"
}

teardown() {
  # A call the fakes don't understand: the test isn't testing what it says.
  if [[ -e "$FAKE/unexpected" ]]; then
    cat "$FAKE/unexpected"
    return 1
  fi
}

# with_secrets PREFIX [SUFFIX...]: each Gitea secret under PREFIX has a
# version, "stored-<suffix>", except the SUFFIXes named.
with_secrets() {
  local prefix=$1 s
  shift
  for s in $SUFFIXES; do
    [[ " $* " == *" $s "* ]] && continue
    printf 'stored-%s' "$s" >"$FAKE/secrets/$prefix-$s"
  done
}

nothing_generated() {
  [ ! -e "$FAKE/generated.log" ]
  [ ! -e "$FAKE/added.log" ]
}

# --- prod: secrets from scripts/bootstrap.sh, never made here ---------------

@test "on prod, every secret present: it renders app.ini from them and adds nothing" {
  with_secrets gitea
  run bash "$SCRIPT" app-ini p gitea "$IMAGE"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = changed ]
  for s in $SUFFIXES; do
    rg -qF "= stored-$s" "$APP_INI"
  done
  [ "$(stat -c %a "$APP_INI")" = 400 ]
  [[ "$(cat "$FAKE/chown.log")" == "1000:1000 $APP_INI."* ]]
  # The diff it prints is masked: a value is never printed.
  [[ "$output" != *stored-* ]]
  nothing_generated
}

@test "on prod a missing secret fails the render, naming it and pointing to scripts/bootstrap.sh" {
  with_secrets gitea secret-key
  run bash "$SCRIPT" app-ini p gitea "$IMAGE"
  [ "$status" -eq 3 ]
  [[ "$output" == *"secret gitea-secret-key is unavailable -- app.ini not rendered"* ]]
  [[ "$output" == *"scripts/bootstrap.sh"* ]]
  [[ "$output" == *"gcloud secrets versions list gitea-secret-key"* ]]
  [ ! -e "$APP_INI" ]
}

@test "on prod a missing secret is never generated, though a version could be added" {
  with_secrets gitea internal-token
  run bash "$SCRIPT" app-ini p gitea "$IMAGE"
  nothing_generated
  [ "$status" -eq 3 ]
  [ ! -e "$APP_INI" ]
}

@test "on prod an empty secret counts as missing" {
  with_secrets gitea
  : >"$FAKE/secrets/gitea-lfs-jwt-secret"
  run bash "$SCRIPT" app-ini p gitea "$IMAGE"
  [ "$status" -eq 3 ]
  [[ "$output" == *"secret gitea-lfs-jwt-secret is unavailable"* ]]
  nothing_generated
  [ ! -e "$APP_INI" ]
}

@test "a prefix other than the rehearsal's fails as prod does" {
  with_secrets gitea-staging oauth2-jwt-secret
  run bash "$SCRIPT" app-ini p gitea-staging "$IMAGE"
  nothing_generated
  [ "$status" -eq 3 ]
  [[ "$output" == *"scripts/bootstrap.sh"* ]]
}

# --- the rehearsal VM: scripts/rehearsal.sh creates its secrets empty -------

@test "the rehearsal generates a missing secret with the Gitea image, into its own secret, and renders it" {
  with_secrets gitea-rehearsal internal-token
  run bash "$SCRIPT" app-ini p gitea-rehearsal "$IMAGE"
  [ "$status" -eq 0 ]
  [ "$(cat "$FAKE/generated.log")" = "$IMAGE INTERNAL_TOKEN" ]
  [ "$(cat "$FAKE/added.log")" = "gitea-rehearsal-internal-token p" ]
  rg -qxF "INTERNAL_TOKEN = generated-INTERNAL_TOKEN" "$APP_INI"
  rg -qxF "SECRET_KEY = stored-secret-key" "$APP_INI"
}

@test "the rehearsal without the Gitea image fails the render and adds nothing" {
  rm "$FAKE/image"
  with_secrets gitea-rehearsal secret-key
  run bash "$SCRIPT" app-ini p gitea-rehearsal "$IMAGE"
  [ "$status" -eq 3 ]
  [[ "$output" == *"secret gitea-rehearsal-secret-key is unavailable"* ]]
  nothing_generated
}

@test "--check never generates, even on the rehearsal" {
  with_secrets gitea-rehearsal secret-key
  run bash "$SCRIPT" --check app-ini p gitea-rehearsal "$IMAGE"
  [ "$status" -eq 3 ]
  nothing_generated
  [ ! -e "$APP_INI" ]
}

# --- GITEA_RENDER_ETC is for these tests only ---------------------------------

@test "without GITEA_RENDER_ETC it writes under /etc, and every file it writes goes through it" {
  run rg -c -F 'etc="${GITEA_RENDER_ETC:-/etc}"' "$SCRIPT"
  [ "$status" -eq 0 ]
  [ "$output" = 1 ]
  # No /etc/gitea or /etc/caddy outside a comment.
  run rg -n '^[^#]*/etc/(gitea|caddy)' "$SCRIPT"
  [ "$status" -eq 1 ]
}
