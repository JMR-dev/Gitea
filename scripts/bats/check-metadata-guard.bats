#!/usr/bin/env bats
# scripts/check-metadata-guard.sh --static: that the ruleset and the quadlets
# agree, the part that needs only bash, sed and grep. Run against the bridge-on
# fixture's renders, copied into a temp dir and changed a line at a time. The
# last test runs the live guard too, where namespaces allow it.
# Run: bats scripts/bats (make scripts-check does).

SCRIPT="$BATS_TEST_DIRNAME/../check-metadata-guard.sh"
GOLDEN="$BATS_TEST_DIRNAME/../../vm/ansible/tests/golden/bridge-on"

setup() {
  for f in nftables.conf alloy.container gitea.container caddy.container; do
    cp "$GOLDEN/$f.golden" "$BATS_TEST_TMPDIR/$f"
  done
  # The fixture's addresses, as its renders have them.
  caddy_ip="$(sed -n 's/^IP=//p' "$BATS_TEST_TMPDIR/caddy.container")"
  gitea_ip="$(sed -n 's/^IP=//p' "$BATS_TEST_TMPDIR/gitea.container")"
}

# The static check over the four copies.
guard() {
  run bash "$SCRIPT" --static "$BATS_TEST_TMPDIR/nftables.conf" "$BATS_TEST_TMPDIR/alloy.container" \
    "$BATS_TEST_TMPDIR/gitea.container" "$BATS_TEST_TMPDIR/caddy.container"
}

@test "the bridge-on renders pass, Caddy at its fixed address" {
  guard
  [ "$status" -eq 0 ]
  [[ "$output" == *"caddy.container (${caddy_ip})"* ]]
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

@test "Caddy on a network that only looks like gitea.network fails" {
  sed -i 's/^Network=gitea\.network$/Network=giteaXnetwork/' "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"caddy.container isn't on the gitea network"* ]]
}

@test "Gitea on a network that only looks like gitea.network fails" {
  sed -i 's/^Network=gitea\.network$/Network=gitea-network/' "$BATS_TEST_TMPDIR/gitea.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"gitea.container isn't on the gitea network"* ]]
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

@test "Caddy at Gitea's address fails: podman would refuse the second to start" {
  sed -i "s/^IP=.*/IP=${gitea_ip}/" "$BATS_TEST_TMPDIR/caddy.container"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"Caddy and Gitea have the same address, ${gitea_ip}"* ]]
}

@test "a rule naming Caddy's address fails: Caddy needs the metadata server for DNS-01" {
  sed -i "s/^\(\s*type filter hook forward .*\)$/\1\n\t\tip saddr ${caddy_ip} tcp dport 80 accept/" \
    "$BATS_TEST_TMPDIR/nftables.conf"
  guard
  [ "$status" -eq 1 ]
  [[ "$output" == *"a rule names Caddy's address ${caddy_ip}"* ]]
}

@test "the live guard takes its files as paths relative to where it is run" {
  # What the live guard needs: a user and network namespace, nft, and
  # ip_forward writable in there. A PR's job pod mounts /proc/sys read-only.
  unshare --user --map-root-user --net sh -c 'echo 1 >/proc/sys/net/ipv4/ip_forward && nft list ruleset' \
    >/dev/null 2>&1 || skip "needs user and network namespaces, nft and a writable ip_forward"
  cd "$BATS_TEST_TMPDIR"
  run bash "$SCRIPT" nftables.conf alloy.container gitea.container caddy.container
  [ "$status" -eq 0 ]
  [[ "$output" == *"all as they should be"* ]]
}
