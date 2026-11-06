#!/usr/bin/env bash
# Runs the watchtower peer script (vm/ansible/roles/tunnel/files/
# gitea-watchtower-peer) over made-up WireGuard configs and a stubbed gcloud:
# the shapes wg-easy publishes, the ones wg and wg-quick would read differently
# from how they look, and every answer Secret Manager can give. The config runs
# on the VM as root, so a shape that gets past check_peer can take the metadata
# server's route or run a command; and only a missing or empty secret may take
# telemetry down.
#
#   scripts/check-peer-config.sh [gitea-watchtower-peer]
#
# The keys are dummies built here, so no key-shaped text sits in the repo (it
# mirrors to GitHub). Everything happens in a scratch directory it deletes.
# vm/ansible/tests/check runs it, so `make check`, every PR preview and every
# deploy do. Needs bash, awk and coreutils.

# Functions sourced from the script read many of the variables set below.
# shellcheck disable=SC2034

set -euo pipefail
# So `... | refuse` below runs in this shell, and its verdict counts.
shopt -s lastpipe

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${1:-${repo_root}/vm/ansible/roles/tunnel/files/gitea-watchtower-peer}"

# Not "work": the script's main has a local of that name, which the gcloud stub
# below would see instead.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# Sourced, the script only defines its functions (it runs main when executed).
# shellcheck source=/dev/null
source "$script"
# It sets -e and friends itself; keep ours.
set -euo pipefail

# 32 bytes in base64, so wg would take them.
key() { printf "%032d" "$1" | base64; }
priv="$(key 1)" pub="$(key 2)" psk="$(key 3)"

# What wg-easy publishes for an agent peer, as `valid`; the others are this with
# something changed, through sed or printf.
valid() {
  printf '[Interface]\nPrivateKey = %s\nAddress = 10.8.0.130/32\nMTU = 1380\nDNS = 1.1.1.1\n\n' "$priv"
  printf '[Peer]\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = 10.8.0.0/24\n' "$pub" "$psk"
  printf 'PersistentKeepalive = 25\nEndpoint = wg.example.org:51820\n'
}
without() { valid | sed "/^$1 /d"; }
# awk, since sed's `a` would eat a blank at the start of the new line.
plus() { valid | KEY="$1" ADD="$2" awk '{ print } index($0, ENVIRON["KEY"]) == 1 { print ENVIRON["ADD"] }'; }
# Into [Interface], with bytes that won't go in an argument.
inside() { printf '[Interface]\n%b\n' "$1"; valid | sed 1d; }

bad=0
n=0
# A config comes in on stdin. Whatever the verdict, check_peer must print
# nothing, and a refusal must write nothing.
run() {
  cat >"${scratch}/in"
  : >"${scratch}/out"
  n=$((n + 1))
  if check_peer "${scratch}/in" "${scratch}/out" >"${scratch}/printed" 2>&1; then rc=0; else rc=1; fi
  if [[ -s "${scratch}/printed" ]]; then
    echo "printed something: $1"
    bad=1
  fi
}
accept() {
  run "$1"
  if ((rc)); then
    echo "refused: $1"
    bad=1
  fi
}
refuse() {
  run "$1"
  if ((!rc)); then
    echo "accepted: $1"
    bad=1
  elif [[ -s "${scratch}/out" ]]; then
    echo "refused but wrote: $1"
    bad=1
  fi
}
# Exactly this line in the output.
has() {
  if ! grep -qxF -- "$1" "${scratch}/out"; then
    echo "not in the output, after \"${2}\": ${1%% =*}"
    bad=1
  fi
}
hasnt() {
  if grep -qiE -- "$1" "${scratch}/out"; then
    echo "still in the output, after \"${2}\": $1"
    bad=1
  fi
}

valid | accept "wg-easy's config"
has "PrivateKey = ${priv}" "wg-easy's config"
has "PublicKey = ${pub}" "wg-easy's config"
has "PresharedKey = ${psk}" "wg-easy's config"
has "Address = 10.8.0.130/32" "wg-easy's config"
has "AllowedIPs = 10.8.0.0/24" "wg-easy's config"
has "Endpoint = wg.example.org:51820" "wg-easy's config"
hasnt '^DNS' "wg-easy's config"
[[ "$(grep -c '^AllowedIPs' "${scratch}/out")" -eq 1 ]] || { echo "not exactly one AllowedIPs"; bad=1; }
cp "${scratch}/out" "${scratch}/normal"
accept "the normalised config, checked again" <"${scratch}/normal"
cmp -s "${scratch}/out" "${scratch}/normal" || { echo "the normalised config changed when checked again"; bad=1; }

# Normalised: watchtower's MTU and keepalive whatever came, no DNS.
without MTU | accept "no MTU"
has "MTU = 1380" "no MTU"
without PersistentKeepalive | accept "no keepalive"
has "PersistentKeepalive = 25" "no keepalive"
valid | sed 's/^MTU = 1380/MTU = 1280/' | accept "another MTU"
has "MTU = 1380" "another MTU"
hasnt '^MTU = 1280' "another MTU"
valid | sed 's/^PersistentKeepalive = 25/PersistentKeepalive = 0/' | accept "the keepalive off"
has "PersistentKeepalive = 25" "the keepalive off"
without PresharedKey | accept "no preshared key"
hasnt '^PresharedKey' "no preshared key"
without DNS | accept "no DNS line"
valid | sed 's/^DNS = .*/DNS = 1.1.1.1, 8.8.8.8/' | accept "two DNS servers"
hasnt '^DNS' "two DNS servers"
valid | sed 's/^\[Interface\]/[INTERFACE]/; s/^\[Peer\]/[peer]/; s/^PrivateKey/PRIVATEKEY/; s/^AllowedIPs/allowedips/' | accept "keys in any case"
has "AllowedIPs = 10.8.0.0/24" "keys in any case"
has "[Peer]" "keys in any case"
has "PrivateKey = ${priv}" "keys in any case"
valid | sed 's/^Address = /# a comment\nAddress=/; s/^AllowedIPs = 10.8.0.0\/24/  Allowed IPs = 10.8.0.0\/24 # 0.0.0.0\/0/' | accept "blanks and comments"
has "AllowedIPs = 10.8.0.0/24" "blanks and comments"
hasnt '0\.0\.0\.0' "blanks and comments"

# Too wide, by each way of writing it.
valid | sed 's#^AllowedIPs = .*#AllowedIPs = 0.0.0.0/0#' | refuse "a full tunnel"
valid | sed 's#^AllowedIPs = .*#AllowedIPs = 10.8.0.0/24, 0.0.0.0/0#' | refuse "a second route on the line"
valid | sed 's#^AllowedIPs = .*#AllowedIPs = 10.8.0.0/16#' | refuse "a wider tunnel range"
valid | sed 's#^AllowedIPs = .*#AllowedIPs = 169.254.169.254/32#' | refuse "the metadata server as the route"
plus AllowedIPs 'AllowedIPs = 0.0.0.0/0' | refuse "a second AllowedIPs"
plus AllowedIPs 'Allowed IPs = 0.0.0.0/0' | refuse "a second AllowedIPs, with a blank in the name"
plus AllowedIPs 'allowed	ips=169.254.169.254/32' | refuse "a second AllowedIPs, with a tab in the name"
plus Endpoint '[Peer]' | refuse "a second [Peer]"
plus Endpoint '[ Peer ]' | refuse "a second [Peer], with blanks in it"
plus Endpoint '[ pe er ]' | refuse "a second [Peer], split and in lower case"
plus MTU '[Interface]' | refuse "a second [Interface]"
without AllowedIPs | refuse "no AllowedIPs"
without Endpoint | refuse "no Endpoint"
valid | sed 's/^\[Peer\]//' | refuse "no [Peer]"
valid | sed 's/^\[Interface\]//' | refuse "no [Interface]"
{ valid | sed -n '/^\[Peer\]/,$p'; echo; valid | sed -n '1,/^MTU/p'; } | refuse "[Peer] before [Interface]"

# Hooks and settings, by each way of writing them.
for k in PreUp PostUp PreDown PostDown; do
  plus MTU "${k} = touch /tmp/owned" | refuse "${k}"
done
plus MTU 'Post Up = touch /tmp/owned' | refuse "PostUp with a blank in the name"
plus MTU 'POSTUP = touch /tmp/owned' | refuse "PostUp in capitals"
plus MTU 'Table = off' | refuse "Table"
plus MTU 'Fw Mark = 51820' | refuse "FwMark with a blank in the name"
plus MTU 'FwMark = 51820' | refuse "FwMark"
plus MTU 'SaveConfig = true' | refuse "SaveConfig"
plus MTU 'ListenPort = 51820' | refuse "a key on no list"
plus Endpoint 'PostUp = touch /tmp/owned' | refuse "a hook in [Peer]"
plus Endpoint 'DNS = 1.1.1.1' | refuse "DNS in [Peer]"
{ echo 'PostUp = touch /tmp/owned'; valid; } | refuse "a key before any section"
plus MTU 'not a setting' | refuse "a line with no ="

# Bytes that wg or bash's read would strip or read as a blank.
inside 'Post\0000Up = touch /tmp/owned' | refuse "NUL inside PostUp"
inside 'Tab\0000le = off' | refuse "NUL inside Table"
inside 'D\0000NS = 1.1.1.1' | refuse "NUL inside DNS"
inside '\fDNS = 1.1.1.1' | refuse "a form feed before DNS"
inside 'Post\vUp = touch /tmp/owned' | refuse "a vertical tab in PostUp"
inside 'Post\rUp = touch /tmp/owned' | refuse "a carriage return in PostUp"
valid | sed 's/$/\r/' | refuse "CRLF line ends"
inside 'Comment = caf\0303\0251' | refuse "a byte over 0x7e"

# Keys wg would print whole in its error.
valid | sed "s#^PrivateKey = .*#PrivateKey = ${priv}X#" | refuse "a private key one too long"
valid | sed "s#^PrivateKey = .*#PrivateKey = ${priv%=}#" | refuse "a private key with no ="
valid | sed "s#^PrivateKey = .*#PrivateKey = ${priv%??}!=#" | refuse "a private key with a bad character"
valid | sed "s#^PrivateKey = .*#PrivateKey = ${priv%??}B=#" | refuse "a private key whose last character decodes to too many bits"
valid | sed 's/^PublicKey = .*/PublicKey = x/' | refuse "a short public key"
valid | sed 's/^PresharedKey = .*/PresharedKey = x/' | refuse "a short preshared key"
without PrivateKey | refuse "no private key"
without PublicKey | refuse "no public key"
plus PrivateKey "PrivateKey = ${priv}" | refuse "a second private key"

# Addresses and the rest.
valid | sed 's#^Address = .*#Address = 10.8.0.5/32#' | refuse "an address under the agent half"
valid | sed 's#^Address = .*#Address = 169.254.169.254/32#' | refuse "the metadata server's address"
valid | sed 's#^Address = .*#Address = 10.8.0.130/24#' | refuse "an address with a wider mask"
valid | sed 's#^Address = .*#Address = 10.8.0.255/32#' | refuse "the tunnel's broadcast address"
valid | sed 's#^Address = .*#Address = 10.8.0.130/32, fd00::1/128#' | refuse "a second address"
plus Address 'Address = 10.8.0.135/32' | refuse "a second Address line"
without Address | refuse "no Address"
valid | sed 's/^MTU = .*/MTU = big/' | refuse "an MTU that is no number"
valid | sed 's/^PersistentKeepalive = .*/PersistentKeepalive = often/' | refuse "a keepalive that is no number"
valid | sed 's/^Endpoint = .*/Endpoint = $(id)/' | refuse "an endpoint that is no host and port"
valid | sed 's/^Endpoint = .*/Endpoint = [2001:db8::1]:51820/' | accept "an IPv6 endpoint"
has "Endpoint = [2001:db8::1]:51820" "an IPv6 endpoint"
: | refuse "an empty file"

((bad == 0)) || { echo "peer check: check_peer gets these wrong (above)" >&2; exit 1; }
echo "peer check: ${n} configs, all judged right"

# ---------------------------------------------------------------------------
# The whole script, as roles/tunnel runs it, over every answer gcloud can give.
# Only no metadata, or a missing or empty secret, may say off or down; anything
# else says keep and leaves the installed config alone.
# ---------------------------------------------------------------------------
WG_DIR="${scratch}/wireguard"
WG_CONF="${WG_DIR}/wg-watchtower.conf"
WORK_BASE="${scratch}/run"
mkdir -p "$WORK_BASE"
SECRET_NAME=projects/example-observe/secrets/wireguard-example-peer
restorecon() { :; }

# gcloud as it answers each case. fakeGCLOUD marks what may never be printed;
# it also records the arguments, to check the project and secret it was asked.
gcloud() {
  echo "$*" >"${scratch}/gcloud-args"
  case "$SECRET" in
    good) valid ;;
    changed) valid | sed 's/^Endpoint = .*/Endpoint = wg2.example.org:51820/' ;;
    empty) ;;
    blank) printf ' \n\t\n' ;;
    refused) valid | sed 's#^AllowedIPs = .*#AllowedIPs = 0.0.0.0/0#' ;;
    no-secret)
      echo 'ERROR: (gcloud.secrets.versions.access) NOT_FOUND: Secret [projects/1/secrets/x] not found or has no versions. fakeGCLOUD' >&2
      return 1
      ;;
    no-version)
      echo 'ERROR: (gcloud.secrets.versions.access) NOT_FOUND: Secret Version [projects/1/secrets/x/versions/latest] not found. fakeGCLOUD' >&2
      return 1
      ;;
    denied)
      echo "ERROR: (gcloud.secrets.versions.access) PERMISSION_DENIED: Permission 'secretmanager.versions.access' denied (or it may not exist). fakeGCLOUD" >&2
      return 1
      ;;
    disabled)
      echo 'ERROR: (gcloud.secrets.versions.access) FAILED_PRECONDITION: Secret Version [projects/1/secrets/x/versions/3] is in DISABLED state. fakeGCLOUD' >&2
      return 1
      ;;
    5xx)
      echo 'ERROR: (gcloud.secrets.versions.access) UNAVAILABLE: The service is currently unavailable. fakeGCLOUD' >&2
      return 1
      ;;
    network)
      echo 'ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool: Max retries exceeded. fakeGCLOUD' >&2
      return 1
      ;;
    token)
      echo 'ERROR: (gcloud.secrets.versions.access) There was a problem refreshing your current auth tokens. fakeGCLOUD' >&2
      return 1
      ;;
  esac
}

# go WANT ARGS...: runs main as roles/tunnel does, in a shell of its own (it
# sets an EXIT trap), and checks it printed exactly one line, starting WANT,
# and nothing else: no key, nothing of gcloud's.
go() {
  local want="$1" rc=0
  shift
  n=$((n + 1))
  (main "$@") >"${scratch}/stdout" 2>"${scratch}/stderr" || rc=$?
  if ((rc)); then
    echo "exited ${rc}: ${want}, $*"
    bad=1
  fi
  if [[ "$(wc -l <"${scratch}/stdout")" -ne 1 || -s "${scratch}/stderr" ]]; then
    echo "printed more than its one line: ${want}, $*: $(cat "${scratch}/stdout" "${scratch}/stderr")"
    bad=1
  fi
  if ! grep -q "^${want}" "${scratch}/stdout"; then
    echo "said \"$(head -n 1 "${scratch}/stdout")\", not \"${want}\": $*"
    bad=1
  fi
  if grep -q -e fakeGCLOUD -e "$priv" -e "$pub" -e "$psk" "${scratch}/stdout" "${scratch}/stderr"; then
    echo "printed gcloud's error or a key: ${want}, $*"
    bad=1
  fi
  if compgen -G "${WORK_BASE}/*" >/dev/null || compgen -G "${WG_DIR}/.*.??????" >/dev/null; then
    echo "left a temporary file behind: ${want}, $*"
    bad=1
  fi
}
installed_is() {
  if ! grep -qxF "$1" "$WG_CONF" 2>/dev/null; then
    echo "the installed config has no \"${1%% =*}\" line as it should: $2"
    bad=1
  fi
}
old() {
  rm -rf "$WG_DIR"
  mkdir -p "$WG_DIR"
  echo old >"$WG_CONF"
}
untouched() {
  if [[ "$(cat "$WG_CONF" 2>/dev/null)" != old ]]; then
    echo "changed the installed config: $1"
    bad=1
  fi
}

# No metadata: off, without asking anyone.
rm -f "${scratch}/gcloud-args"
SECRET=good
go off
go off ""
[[ ! -e "${scratch}/gcloud-args" ]] || { echo "asked gcloud with no metadata"; bad=1; }

# Metadata that is no resource name: keep, without asking.
for name in wireguard-gitea-vm-peer projects/x/secrets/y "projects/example-observe/secrets/a b" \
  'projects/example-observe/secrets/x;id' projects/example-observe/secrets/x/versions/1; do
  old
  go keep "$name"
  untouched "metadata ${name}"
done
[[ ! -e "${scratch}/gcloud-args" ]] || { echo "asked gcloud with a malformed name"; bad=1; }

# A first config, then the same again, then another: installed only when it
# changes, private, and asked of the right project and secret.
rm -rf "$WG_DIR"
SECRET=good
go "up changed" "$SECRET_NAME"
grep -qx -- 'secrets versions access latest --secret=wireguard-example-peer --project=example-observe' \
  "${scratch}/gcloud-args" || { echo "asked gcloud: $(cat "${scratch}/gcloud-args")"; bad=1; }
installed_is "PrivateKey = ${priv}" "a first config"
installed_is "MTU = 1380" "a first config"
[[ "$(stat -c %a "$WG_CONF")" == 600 && "$(stat -c %a "$WG_DIR")" == 700 ]] \
  || { echo "the config or its directory is not private: $(stat -c '%a %n' "$WG_DIR" "$WG_CONF")"; bad=1; }
go "up unchanged" "$SECRET_NAME"
go "up unchanged" --check "$SECRET_NAME"
SECRET=changed
cp "$WG_CONF" "${scratch}/before"
go "up would-change" --check "$SECRET_NAME"
cmp -s "$WG_CONF" "${scratch}/before" || { echo "--check installed the changed config"; bad=1; }
go "up changed" "$SECRET_NAME"
installed_is "Endpoint = wg2.example.org:51820" "a changed config"

# --check on a VM with no config yet: says so, writes nothing.
rm -rf "$WG_DIR"
SECRET=good
go "up would-change" --check "$SECRET_NAME"
[[ ! -e "$WG_DIR" ]] || { echo "--check made ${WG_DIR}"; bad=1; }

# No peer: down. The script removes nothing itself; roles tunnel and logship do.
for SECRET in no-secret no-version empty blank; do
  old
  go down "$SECRET_NAME"
  go down --check "$SECRET_NAME"
done

# Anything else: keep, and the config stays as it is.
for SECRET in denied disabled 5xx network token refused; do
  old
  go keep "$SECRET_NAME"
  untouched "secret: ${SECRET}"
done
SECRET=denied
go "keep could not read ${SECRET_NAME}: permission denied" "$SECRET_NAME"
SECRET=token
go "keep could not read ${SECRET_NAME}: a network, API or credentials error" "$SECRET_NAME"
SECRET=refused
go "keep the peer config in ${SECRET_NAME} failed its checks" "$SECRET_NAME"

# Too many arguments: a usage error, exit 2, nothing on stdout.
rc=0
(main a b) >"${scratch}/stdout" 2>/dev/null || rc=$?
n=$((n + 1))
[[ "$rc" -eq 2 && ! -s "${scratch}/stdout" ]] || { echo "two arguments: rc ${rc}"; bad=1; }

((bad == 0)) || { echo "peer check: gitea-watchtower-peer gets these wrong (above)" >&2; exit 1; }
echo "peer check: ${n} checks in all, telemetry stops only when it should"
