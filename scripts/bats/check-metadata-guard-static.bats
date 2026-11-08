#!/usr/bin/env bats
# scripts/check-metadata-guard.sh --static: which Network= lines in
# gitea.container count as "on the gitea network".
#
# The guard's forward rule protects Gitea only if Gitea really is on the gitea
# network at the address the rule names. Gitea's quadlet carries its fixed MAC
# as a network option (Network=gitea.network:mac=...), so the guard accepts
# that one option and nothing else: no ip=/ip6= (an address the forward rule
# doesn't name), no other option, not a look-alike name, not another network,
# not trailing text.
#
# Each test copies the bridge-on fixture's real renders and changes only
# gitea.container's Network= line. GUARD points at another copy of the script
# (for example main's) to show these tests failing against it.
# Run: bats scripts/bats/check-metadata-guard-static.bats

GOLDEN="$BATS_TEST_DIRNAME/../../vm/ansible/tests/golden/bridge-on"
GUARD="${GUARD:-$BATS_TEST_DIRNAME/../check-metadata-guard.sh}"

setup() {
  for f in nftables.conf alloy.container gitea.container caddy.container; do
    cp "$GOLDEN/$f.golden" "$BATS_TEST_TMPDIR/$f"
  done
}

# gitea_network <line>: replace gitea.container's Network= line with <line>
# (or remove it when <line> is empty).
gitea_network() {
  local unit="$BATS_TEST_TMPDIR/gitea.container" line=$1
  rg -q '^Network=' "$unit"
  if [ -z "$line" ]; then
    sed -i '/^Network=/d' "$unit"
  else
    sed -i "s|^Network=.*|${line}|" "$unit"
  fi
}

guard() {
  run bash "$GUARD" --static "$BATS_TEST_TMPDIR/nftables.conf" "$BATS_TEST_TMPDIR/alloy.container" \
    "$BATS_TEST_TMPDIR/gitea.container" "$BATS_TEST_TMPDIR/caddy.container"
}

refused_as_off_network() {
  [ "$status" -eq 1 ]
  [[ "$output" == *"gitea.container isn't on the gitea network"* ]]
}

@test "the fixture's renders pass, and so does Gitea's fixed MAC on Network=" {
  guard
  [ "$status" -eq 0 ]
  [[ "$output" == *"the ruleset agrees with"* ]]

  gitea_network 'Network=gitea.network:mac=02:00:00:00:00:fa'
  rg -qxF 'Network=gitea.network:mac=02:00:00:00:00:fa' "$BATS_TEST_TMPDIR/gitea.container"
  guard
  [ "$status" -eq 0 ]
  [[ "$output" == *"the ruleset agrees with"* ]]
}

@test "the plain network name, with no options, passes" {
  gitea_network 'Network=gitea.network'
  guard
  [ "$status" -eq 0 ]
}

@test "an ip= option beside the MAC is refused: it could give Gitea an address the rule doesn't name" {
  gitea_network 'Network=gitea.network:mac=02:00:00:00:00:fa,ip=10.89.10.99'
  guard
  refused_as_off_network
}

@test "an ip= or ip6= option alone is refused" {
  gitea_network 'Network=gitea.network:ip=10.89.10.99'
  guard
  refused_as_off_network

  gitea_network 'Network=gitea.network:ip6=fd00::99'
  guard
  refused_as_off_network
}

@test "any option other than mac= is refused" {
  gitea_network 'Network=gitea.network:interface_name=eth0'
  guard
  refused_as_off_network
}

@test "a mac= that isn't six lowercase hex pairs is refused" {
  gitea_network 'Network=gitea.network:mac=02:00:00:00:fa'
  guard
  refused_as_off_network

  gitea_network 'Network=gitea.network:mac=02:00:00:00:00:FA'
  guard
  refused_as_off_network

  gitea_network 'Network=gitea.network:mac=02:00:00:00:00:fa:'
  guard
  refused_as_off_network
}

@test "a look-alike name with a dash instead of the dot is refused" {
  gitea_network 'Network=gitea-network'
  guard
  refused_as_off_network
}

@test "a look-alike name with another character for the dot, and options, is refused" {
  gitea_network 'Network=giteaXnetwork:mac=02:00:00:00:00:fa'
  guard
  refused_as_off_network
}

@test "a longer name that starts with gitea.network is refused" {
  gitea_network 'Network=gitea.networks:mac=02:00:00:00:00:fa'
  guard
  refused_as_off_network
}

@test "an empty option list (a bare colon) is refused" {
  gitea_network 'Network=gitea.network:'
  guard
  refused_as_off_network
}

@test "trailing text after a space is refused" {
  gitea_network 'Network=gitea.network extra'
  guard
  refused_as_off_network
}

@test "another network, or the host network, is refused" {
  gitea_network 'Network=other.network:mac=02:00:00:00:00:fa'
  guard
  refused_as_off_network

  gitea_network 'Network=host'
  guard
  refused_as_off_network
}

@test "no Network= line at all is refused" {
  gitea_network ''
  guard
  refused_as_off_network
}
