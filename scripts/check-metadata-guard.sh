#!/usr/bin/env bash
# Checks the metadata guard: vm/nftables/gitea.nft keeps Alloy's uid off the
# metadata server (169.254.169.254), whose token is the VM's credentials, but
# lets it through for DNS, and lets every other uid through for anything.
# Ported from the blog's scripts/check-metadata-guard.sh.
#
#   scripts/check-metadata-guard.sh [--static] [nftables.conf] [alloy.container]
#   (defaults: vm/ansible/tests/golden/bridge-on/nftables.conf.golden, the
#   ruleset as rendered, and vm/quadlets/alloy.container)
#
# First, always, that the rule and the quadlet agree: Alloy runs as the uid
# the two rules name, on the host network, with no user namespace, and nothing
# else in the ruleset touches the metadata server (Caddy on the host network
# needs it for DNS-01). --static stops there; that part needs only bash, sed
# and grep, so vm/ansible/tests/check runs it everywhere, deploys included.
#
# Then it loads the ruleset in a throwaway user and network namespace and
# probes the address as each uid. Nothing on the host is touched: the ruleset,
# the address and the listeners all go with the namespace. Where this user has
# subordinate uids (unshare --map-auto), the probes really run as uids 473,
# 1000 and root. Without them the namespace has root alone: the ruleset is
# checked as written (root gets through), then with the rule's uid changed to
# root's, so the rule is still seen to fire, and this says so. It can't show
# what a namespace doesn't have: the real metadata server, and a rootful podman
# container on the host network. `make check` runs it; it needs unshare,
# setpriv, nft, ip and python3.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
static=0
if [[ "${1:-}" == --static ]]; then
  static=1
  shift
fi
ruleset="${1:-${repo_root}/vm/ansible/tests/golden/bridge-on/nftables.conf.golden}"
quadlet="${2:-${repo_root}/vm/quadlets/alloy.container}"
metadata=169.254.169.254
table=gitea_filter

fail() { echo "metadata guard: $*" >&2; exit 1; }

# A listener on each port, or a probe of one. The probe exits 0 only on a reply,
# so a UDP packet that goes nowhere isn't taken for one that got through.
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
    # Not from the address itself: the reply, under root when that is the only
    # uid there is, would be going to the metadata address too.
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

# Inside the namespace, as its root. A separate run of this file, so the outside
# stays clean whatever goes wrong in here.
inside() {
  local rendered="$1" single_uid="$2" alloy="$3"
  # Other uids may not be able to read where this was started.
  cd /
  ip link set lo up
  ip addr add "${metadata}/32" dev lo
  nft -f "$rendered"
  nft list chain inet "$table" output >/dev/null || fail "no output chain after the load"

  # Alone in this namespace, so no one else's listener or port.
  python3 -c "$probe_py" serve "$metadata" &
  local server=$!
  # shellcheck disable=SC2064
  trap "kill ${server} 2>/dev/null" EXIT
  local _
  for _ in $(seq 100); do
    python3 -c "$probe_py" try "$metadata" tcp 80 && break
    sleep 0.1
  done

  # Whoever isn't Alloy goes anywhere.
  probe 0 tcp 80 reaches
  probe 0 tcp 443 reaches
  probe 0 tcp 53 reaches
  probe 0 udp 53 reaches
  probe 0 udp 123 reaches

  if ((single_uid)); then
    # Only root exists in here. The same file, with the rule aimed at root.
    sed "s/meta skuid ${alloy} /meta skuid 0 /" "$rendered" >"${rendered}.root"
    grep -q 'meta skuid 0 ' "${rendered}.root" || fail "no skuid rule to aim at root"
    nft -f "${rendered}.root"
    alloy=0
  else
    # Caddy's uid on the host network, which needs the server for DNS-01, and
    # the uid next to Alloy's, so the rule is seen to name one uid only.
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

  ((bad == 0)) || exit 1
  # The refusals were the rule's, not a dead listener.
  nft list chain inet "$table" output | grep -qE 'counter packets [1-9]' \
    || fail "the rule's counter never moved"
  echo "$n"
}

if [[ "${1:-}" == --inside ]]; then
  shift
  inside "$@"
  exit
fi

[[ -f "$ruleset" && -f "$quadlet" ]] || fail "need ${ruleset} and ${quadlet}"

# The rule is worth nothing if the uid moves and it doesn't.
quadlet_uid="$(sed -n 's/^User=//p' "$quadlet")"
[[ "$quadlet_uid" =~ ^[1-9][0-9]*$ ]] || fail "alloy.container runs as ${quadlet_uid:-nobody}, not a fixed non-root uid"
if grep -q '^UserNS=' "$quadlet"; then
  fail "alloy.container has a UserNS=, so its uid on the host isn't ${quadlet_uid}"
fi
grep -q '^Network=host$' "$quadlet" || fail "alloy.container isn't on the host network, so its packets never reach the output chain"
[[ "$(grep -cE "^\s*meta skuid ${quadlet_uid} ip daddr ${metadata//./\\.} " "$ruleset")" -eq 2 ]] \
  || fail "the ruleset should have two rules for uid ${quadlet_uid}, Alloy's in alloy.container"
if grep -E "^\s*[^#[:space:]].*\bip daddr ${metadata//./\\.}\b" "$ruleset" | grep -qv "meta skuid ${quadlet_uid} "; then
  fail "a rule on the metadata server for someone other than Alloy: Caddy needs it for DNS-01"
fi
if ((static)); then
  echo "metadata guard: the ruleset and alloy.container agree on uid ${quadlet_uid}"
  exit 0
fi

self="${BASH_SOURCE[0]}"
if unshare --user --map-auto --map-root-user --net true 2>/dev/null \
    && unshare --user --map-auto --map-root-user --net setpriv --reuid "$quadlet_uid" --regid "$quadlet_uid" --clear-groups true 2>/dev/null; then
  count="$(unshare --user --map-auto --map-root-user --net "$self" --inside "$ruleset" 0 "$quadlet_uid")"
  echo "metadata guard: ${count} probes in a namespace as uids ${quadlet_uid}, $((quadlet_uid + 1)), 1000 and 0: all as they should be"
elif unshare --user --map-root-user --net true 2>/dev/null; then
  count="$(unshare --user --map-root-user --net "$self" --inside "$ruleset" 1 "$quadlet_uid")"
  echo "metadata guard: ${count} probes in a namespace as root only (no subordinate uids here), the rule aimed at root: all as they should be"
else
  fail "can't make a user and network namespace here"
fi
