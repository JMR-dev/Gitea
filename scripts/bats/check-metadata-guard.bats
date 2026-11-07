#!/usr/bin/env bats
# scripts/check-metadata-guard.sh --static: that the ruleset and the quadlets
# agree, the part that needs only bash, sed and grep. Run against the bridge-on
# fixture's renders, copied into a temp dir and changed a line at a time.
# Run: bats scripts/bats (make scripts-check does).

SCRIPT="$BATS_TEST_DIRNAME/../check-metadata-guard.sh"
GOLDEN="$BATS_TEST_DIRNAME/../../vm/ansible/tests/golden/bridge-on"

setup() {
  for f in nftables.conf alloy.container gitea.container caddy.container; do
    cp "$GOLDEN/$f.golden" "$BATS_TEST_TMPDIR/$f"
  done
}

# The static check over the four copies.
guard() {
  run bash "$SCRIPT" --static "$BATS_TEST_TMPDIR/nftables.conf" "$BATS_TEST_TMPDIR/alloy.container" \
    "$BATS_TEST_TMPDIR/gitea.container" "$BATS_TEST_TMPDIR/caddy.container"
}

@test "the bridge-on renders pass, Caddy at its fixed address" {
  guard
  [ "$status" -eq 0 ]
  [[ "$output" == *"caddy.container (10.89.10.251)"* ]]
}

@test "with no files named, it checks the bridge-on renders" {
  run bash "$SCRIPT" --static
  [ "$status" -eq 0 ]
}

@test "Caddy on the host network fails, with no IP= as host mode rendered it" {
  sed -i -e 's/^Network=gitea\.network$/Network=host/' -e '/^IP=/d' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container is on the host network: Caddy runs on the gitea network only"* ]]
}

@test "Caddy on the host network fails with an IP= as well" {
  sed -i 's/^Network=gitea\.network$/Network=host/' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container is on the host network: Caddy runs on the gitea network only"* ]]
}

@test "Caddy on some other network fails" {
  sed -i 's/^Network=gitea\.network$/Network=other.network/' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container isn't on the gitea network"* ]]
}

@test "Caddy with no Network= line fails" {
  sed -i '/^Network=/d' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container isn't on the gitea network"* ]]
}

@test "Caddy with no fixed IP= fails" {
  sed -i '/^IP=/d' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container has no fixed IP="* ]]
}

@test "a rule naming Caddy's address fails: Caddy needs the metadata server for DNS-01" {
  sed -i 's/^\(\s*type filter hook forward .*\)$/\1\n\t\tip saddr 10.89.10.251 tcp dport 80 accept/' \
    "$BATS_TEST_TMPDIR/nftables.conf"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"a rule names Caddy's address 10.89.10.251"* ]]
}
