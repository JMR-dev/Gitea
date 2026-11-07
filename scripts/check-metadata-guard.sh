#!/usr/bin/env bash
# Checks the metadata guard in vm/nftables/gitea.nft, which keeps the metadata
# server (169.254.169.254), whose token is the VM's credentials, away from the
# two things that don't need it:
#   - Alloy (uid 473 on the host network), through the output chain, but for
#     DNS;
#   - the Gitea container (its fixed address on the gitea network), through
#     the forward chain;
# and from nothing else: other uids on the host, Caddy's address on the gitea
# network (it needs the server for DNS-01) and any other address there keep
# it. The Alloy half is ported from the blog's scripts/check-metadata-guard.sh.
# scripts/bats/check-metadata-guard.bats tests the --static part.
#
#   scripts/check-metadata-guard.sh [--static] [nftables.conf] [alloy.container]
#                                   [gitea.container] [caddy.container]
#   (defaults: the bridge-on fixture's renders, vm/ansible/tests/golden/bridge-on/*.golden)
#
# First, always, that the rules and the quadlets agree:
#   - Alloy runs as the uid the two output rules name, on the host network,
#     with no user namespace;
#   - Gitea has a fixed address (IP=) on the gitea network, the one the forward
#     rule names, and no other rule names it;
#   - Caddy is on the gitea network, never the host's, with a fixed address of
#     its own (IP=), which no rule names;
#   - the forward chain's policy is accept, and nothing else in the ruleset
#     touches the metadata server.
# --static stops there; that part needs only bash, sed, grep and realpath, so
# vm/ansible/tests/check runs it everywhere, deploys included.
#
# Then it loads the ruleset in a throwaway user and network namespace and
# probes the address: as each uid on the "host", and from a "container"
# namespace on a veth to it, with the server in a namespace of its own beyond,
# so that traffic is forwarded as a bridged container's is, once from Gitea's
# address and once from others. Nothing on the host is touched: the ruleset,
# the addresses and the listeners all go with the namespaces. Where this user
# has subordinate uids (unshare --map-auto), the uid probes really run as uids
# 473, 474, 1000 and root. Without them the namespace has root alone: the
# ruleset is checked as written (root gets through), then with the uid rule
# changed to root's, so it is still seen to fire, and this says so. It can't
# show what a namespace doesn't have: the real metadata server, and podman's
# own containers and netavark's rules. `make check` runs it; it needs unshare,
# nsenter, setpriv, nft, ip and python3.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
golden="${repo_root}/vm/ansible/tests/golden/bridge-on"
static=0
if [[ "${1:-}" == --static ]]; then
  static=1
  shift
fi
ruleset="${1:-${golden}/nftables.conf.golden}"
alloy_quadlet="${2:-${golden}/alloy.container.golden}"
gitea_quadlet="${3:-${golden}/gitea.container.golden}"
caddy_quadlet="${4:-${golden}/caddy.container.golden}"
metadata=169.254.169.254
table=gitea_filter

fail() { echo "metadata guard: $*" >&2; exit 1; }

# A listener on each port, or a probe of one. The probe exits 0 only on a reply,
# so a UDP packet that goes nowhere isn't taken for one that got through.
# "try" binds to loopback first; "tryfrom" doesn't, for a namespace whose own
# address must be the source.
probe_py='
import socket, sys, threading

def serve_tcp(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((sys.argv[2], port))
    s.listen()
    while True:
        c, _ = s.accept()
        c.sendall(b"ok")
        c.close()

def serve_udp(port):
    s = socket.socket(type=socket.SOCK_DGRAM)
    s.bind((sys.argv[2], port))
    while True:
        _, peer = s.recvfrom(16)
        s.sendto(b"ok", peer)

if sys.argv[1] == "serve":
    for port in (53, 80, 443):
        threading.Thread(target=serve_tcp, args=(port,), daemon=True).start()
    for port in (53, 123):
        threading.Thread(target=serve_udp, args=(port,), daemon=True).start()
    threading.Event().wait()

proto, port = sys.argv[3], int(sys.argv[4])
s = socket.socket(type=socket.SOCK_DGRAM if proto == "udp" else socket.SOCK_STREAM)
s.settimeout(2)
try:
    if sys.argv[1] == "try":
        # Not from the address itself: the reply, under root when that is the
        # only uid there is, would be going to the metadata address too.
        s.bind(("127.0.0.1", 0))
    s.connect((sys.argv[2], port))
    if proto == "udp":
        s.send(b"x")
    sys.exit(0 if s.recv(16) == b"ok" else 1)
except OSError:
    sys.exit(1)
'

bad=0
n=0
# probe <uid> <proto> <port> <reaches|refused>
probe() {
  local uid="$1" proto="$2" port="$3" want="$4" got=refused
  # Already that uid, nothing to change. With root alone mapped, setgroups is
  # denied, so setpriv would fail and read as a refusal.
  local become=(setpriv --reuid "$uid" --regid "$uid" --clear-groups)
  [[ "$uid" != "$(id -u)" ]] || become=()
  n=$((n + 1))
  if "${become[@]}" python3 -c "$probe_py" try "$metadata" "$proto" "$port" >/dev/null 2>&1; then
    got=reaches
  fi
  if [[ "$got" != "$want" ]]; then
    echo "uid ${uid} ${proto}/${port}: ${got}, wanted ${want}" >&2
    bad=1
  fi
}

# probe_from <address> <proto> <port> <reaches|refused>: from the container
# namespace, with that one address on its veth, so the packet is forwarded.
probe_from() {
  local addr="$1" proto="$2" port="$3" want="$4" got=refused
  nsenter --net="/proc/${ctr}/ns/net" ip addr flush dev ctr1
  nsenter --net="/proc/${ctr}/ns/net" ip addr add "${addr}/24" dev ctr1
  nsenter --net="/proc/${ctr}/ns/net" ip route replace default via "$gateway" dev ctr1
  n=$((n + 1))
  if nsenter --net="/proc/${ctr}/ns/net" python3 -c "$probe_py" tryfrom "$metadata" "$proto" "$port" >/dev/null 2>&1; then
    got=reaches
  fi
  if [[ "$got" != "$want" ]]; then
    echo "from ${addr}, forwarded, ${proto}/${port}: ${got}, wanted ${want}" >&2
    bad=1
  fi
}

# Inside the namespace, as its root. A separate run of this file, so the outside
# stays clean whatever goes wrong in here.
inside() {
  local rendered="$1" single_uid="$2" alloy="$3" gitea="$4" caddy="$5"
  # Other uids may not be able to read where this was started.
  cd /
  ip link set lo up
  ip addr add "${metadata}/32" dev lo
  nft -f "$rendered"
  nft list chain inet "$table" output >/dev/null || fail "no output chain after the load"
  nft list chain inet "$table" forward >/dev/null || fail "no forward chain after the load"

  # Alone in this namespace, so no one else's listener or port.
  python3 -c "$probe_py" serve "$metadata" &
  local server=$!
  # Global, not local: the trap reads it after this function has returned.
  helpers=("$server")
  trap 'kill "${helpers[@]}" 2>/dev/null' EXIT
  local _
  for _ in $(seq 100); do
    python3 -c "$probe_py" try "$metadata" tcp 80 && break
    sleep 0.1
  done

  # The host. Whoever isn't Alloy goes anywhere.
  probe 0 tcp 80 reaches
  probe 0 tcp 443 reaches
  probe 0 tcp 53 reaches
  probe 0 udp 53 reaches
  probe 0 udp 123 reaches

  if ((single_uid)); then
    # Only root exists in here. The same file, with the uid rule aimed at root.
    sed "s/meta skuid ${alloy} /meta skuid 0 /" "$rendered" >"${rendered}.root"
    grep -q 'meta skuid 0 ' "${rendered}.root" || fail "no skuid rule to aim at root"
    nft -f "${rendered}.root"
    alloy=0
  else
    # Other uids keep the server: 1000 (Caddy's and Gitea's inside their
    # containers, whose traffic is forwarded and never comes here, so on the
    # host just another uid) and the one next to Alloy's, so the rule is seen
    # to name one uid only.
    probe 1000 tcp 80 reaches
    probe 1000 tcp 443 reaches
    probe "$((alloy + 1))" tcp 80 reaches
  fi

  # Alloy: DNS through, and nothing else.
  probe "$alloy" tcp 53 reaches
  probe "$alloy" udp 53 reaches
  probe "$alloy" tcp 80 refused
  probe "$alloy" tcp 443 refused
  probe "$alloy" udp 123 refused

  # Read whole, then searched: `nft ... | grep -q` under pipefail fails when grep
  # stops reading first and nft dies of SIGPIPE.
  local chain
  chain="$(nft list chain inet "$table" output)"
  grep -qE 'counter packets [1-9]' <<<"$chain" || fail "the output rule's counter never moved"

  # Now the bridge. The server moves to a namespace of its own, so that what a
  # container sends it is forwarded through this one, past the forward chain,
  # as a bridged container's traffic is on the VM.
  kill "$server" 2>/dev/null || true
  ip addr del "${metadata}/32" dev lo
  gateway="${gitea%.*}.1"
  unshare --net sleep 600 &
  ctr=$!
  unshare --net sleep 600 &
  local mds=$!
  helpers+=("$ctr" "$mds")
  sleep 0.2
  local m=(nsenter --net="/proc/${mds}/ns/net")
  ip link add ctr0 type veth peer name ctr1
  ip link set ctr1 netns "$ctr"
  ip link add md0 type veth peer name md1
  ip link set md1 netns "$mds"
  ip addr add "${gateway}/24" dev ctr0
  ip link set ctr0 up
  ip addr add 192.0.2.1/30 dev md0
  ip link set md0 up
  ip route add "${metadata}/32" via 192.0.2.2 dev md0
  echo 1 >/proc/sys/net/ipv4/ip_forward
  nsenter --net="/proc/${ctr}/ns/net" ip link set lo up
  nsenter --net="/proc/${ctr}/ns/net" ip link set ctr1 up
  "${m[@]}" ip link set lo up
  "${m[@]}" ip addr add 192.0.2.2/30 dev md1
  "${m[@]}" ip link set md1 up
  "${m[@]}" ip addr add "${metadata}/32" dev lo
  "${m[@]}" ip route add "${gateway%.*}.0/24" via 192.0.2.1 dev md1
  "${m[@]}" python3 -c "$probe_py" serve "$metadata" &
  helpers+=("$!")

  # Another bridge address first, so a refusal later is the rule's and not a
  # path that never worked.
  local other="${gateway%.*}.2"
  for _ in $(seq 100); do
    nsenter --net="/proc/${ctr}/ns/net" ip addr flush dev ctr1
    nsenter --net="/proc/${ctr}/ns/net" ip addr add "${other}/24" dev ctr1
    nsenter --net="/proc/${ctr}/ns/net" ip route replace default via "$gateway" dev ctr1
    nsenter --net="/proc/${ctr}/ns/net" python3 -c "$probe_py" tryfrom "$metadata" tcp 80 && break
    sleep 0.1
  done
  probe_from "$other" tcp 80 reaches
  probe_from "$other" udp 53 reaches
  # Caddy's: DNS-01 needs the server.
  probe_from "$caddy" tcp 80 reaches
  probe_from "$caddy" tcp 443 reaches
  # Gitea's: nothing at all.
  probe_from "$gitea" tcp 80 refused
  probe_from "$gitea" tcp 443 refused
  probe_from "$gitea" tcp 53 refused
  probe_from "$gitea" udp 53 refused
  # And the address next to it, so the rule is seen to name one address only.
  probe_from "${gitea%.*}.$(( ${gitea##*.} - 1 ))" tcp 80 reaches

  ((bad == 0)) || exit 1
  chain="$(nft list chain inet "$table" forward)"
  grep -qE 'counter packets [1-9]' <<<"$chain" || fail "the forward rule's counter never moved"
  echo "$n"
}

if [[ "${1:-}" == --inside ]]; then
  shift
  inside "$@"
  exit
fi

for f in "$ruleset" "$alloy_quadlet" "$gitea_quadlet" "$caddy_quadlet"; do
  [[ -f "$f" ]] || fail "need ${f}"
done
# Absolute, as the live part runs this file again inside the namespaces, and
# that run starts with cd /.
ruleset="$(realpath "$ruleset")"
alloy_quadlet="$(realpath "$alloy_quadlet")"
gitea_quadlet="$(realpath "$gitea_quadlet")"
caddy_quadlet="$(realpath "$caddy_quadlet")"
md="${metadata//./\\.}"
# The ruleset's rules, comments and blank lines left out.
rules="$(sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$ruleset")"

# Alloy: the rule is worth nothing if the uid moves and it doesn't.
alloy_uid="$(sed -n 's/^User=//p' "$alloy_quadlet")"
[[ "$alloy_uid" =~ ^[1-9][0-9]*$ ]] || fail "alloy.container runs as ${alloy_uid:-nobody}, not a fixed non-root uid"
if grep -q '^UserNS=' "$alloy_quadlet"; then
  fail "alloy.container has a UserNS=, so its uid on the host isn't ${alloy_uid}"
fi
grep -q '^Network=host$' "$alloy_quadlet" || fail "alloy.container isn't on the host network, so its packets never reach the output chain"
[[ "$(grep -cE "^\s*meta skuid ${alloy_uid} ip daddr ${md} " <<<"$rules")" -eq 2 ]] \
  || fail "the ruleset should have two rules for uid ${alloy_uid}, Alloy's in alloy.container"

# Gitea: a fixed address on the gitea network, the one the forward rule names.
gitea_ip="$(sed -n 's/^IP=//p' "$gitea_quadlet")"
[[ "$gitea_ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail "gitea.container has no fixed IP= (got '${gitea_ip}')"
grep -qxF 'Network=gitea.network' "$gitea_quadlet" || fail "gitea.container isn't on the gitea network"
gi="${gitea_ip//./\\.}"
[[ "$(grep -cE "^\s*ip saddr ${gi} ip daddr ${md} .*\breject\b" <<<"$rules")" -eq 1 ]] \
  || fail "the ruleset should have one rule rejecting ${gitea_ip}, gitea.container's IP=, to the metadata server"
[[ "$(grep -cE "(^|[^0-9.])${gi}([^0-9]|$)" <<<"$rules")" -eq 1 ]] \
  || fail "${gitea_ip} is named by more than that one rule"
grep -qE '^\s*type filter hook forward priority filter; policy accept;' <<<"$rules" \
  || fail "no forward chain with an accept policy: a drop policy would drop every container's traffic"
if grep -qE 'hook forward .*policy drop' <<<"$rules"; then
  fail "a forward chain with a drop policy: it would drop every container's traffic"
fi

# Caddy: on the gitea network, never the host's, with an address of its own,
# which nothing names. On the host network its loopback, its admin API among
# it, would be every host process's too.
if grep -q '^Network=host$' "$caddy_quadlet"; then
  fail "caddy.container is on the host network: Caddy runs on the gitea network only"
fi
grep -qxF 'Network=gitea.network' "$caddy_quadlet" || fail "caddy.container isn't on the gitea network"
caddy_ip="$(sed -n 's/^IP=//p' "$caddy_quadlet")"
[[ "$caddy_ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail "caddy.container has no fixed IP= (got '${caddy_ip}')"
[[ "$caddy_ip" != "$gitea_ip" ]] || fail "Caddy and Gitea have the same address, ${gitea_ip}"
if grep -qE "(^|[^0-9.])${caddy_ip//./\\.}([^0-9]|$)" <<<"$rules"; then
  fail "a rule names Caddy's address ${caddy_ip}: Caddy needs the metadata server for DNS-01"
fi

# And nothing else touches the metadata server: not a whole subnet, not root.
[[ "$(grep -cE "\bip daddr ${md}\b" <<<"$rules")" -eq 3 ]] \
  || fail "a rule on the metadata server other than Alloy's two and Gitea's one"

if ((static)); then
  echo "metadata guard: the ruleset agrees with alloy.container (uid ${alloy_uid}), gitea.container (${gitea_ip}) and caddy.container (${caddy_ip})"
  exit 0
fi

self="${BASH_SOURCE[0]}"
if unshare --user --map-auto --map-root-user --net true 2>/dev/null \
    && unshare --user --map-auto --map-root-user --net setpriv --reuid "$alloy_uid" --regid "$alloy_uid" --clear-groups true 2>/dev/null; then
  count="$(unshare --user --map-auto --map-root-user --net "$self" --inside "$ruleset" 0 "$alloy_uid" "$gitea_ip" "$caddy_ip")"
  echo "metadata guard: ${count} probes in namespaces, as uids ${alloy_uid}, $((alloy_uid + 1)), 1000 and 0 and forwarded from ${gitea_ip}, ${caddy_ip} and others: all as they should be"
elif unshare --user --map-root-user --net true 2>/dev/null; then
  count="$(unshare --user --map-root-user --net "$self" --inside "$ruleset" 1 "$alloy_uid" "$gitea_ip" "$caddy_ip")"
  echo "metadata guard: ${count} probes in namespaces as root only (no subordinate uids here, the uid rule aimed at root), and forwarded from ${gitea_ip}, ${caddy_ip} and others: all as they should be"
else
  fail "can't make a user and network namespace here"
fi
