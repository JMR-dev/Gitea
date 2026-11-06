#!/usr/bin/env bash
# Runs a client config's scrub rules over a fixture with Alloy itself, and
# checks what would ship. Nothing on watchtower scrubs, so these rules are the
# only thing between a service's logs and storage (docs/client-telemetry.md).
#
#   scripts/check-alloy-scrub.sh [config] [fixture]
#   (defaults: config/alloy/config.alloy.example, config/alloy/testdata/scrub.log)
#
# The rules are the loki.process block between the config's
# "// scrub: begin" and "// scrub: end" lines; they run with the fixture's
# lines in and loki.echo out. Each fixture line carries a tag:
#   keep:<n>   comes out (scrubbed)
#   same:<n>   comes out unchanged (a rule mustn't mangle ordinary lines)
#   drop:<n>   doesn't come out
# and nothing that comes out may match a line of <fixture>.forbidden (grep
# -E). The last line mustn't be a drop: the run ends when it comes out.
#
# A Caddy pipeline's JSON check runs only on container="caddy", and a tagged
# line isn't JSON. So <fixture minus .log>.caddy.log, if there is one, runs too,
# under that label, with each line's tag somewhere inside it instead:
#   keep:c<n>  comes out
#   drop:c<n>  doesn't come out
# The same rule holds for its last line.
#
# keep: says a line came out, not what is left of it. <fixture minus .log>.expect,
# if there is one, holds whole lines exactly as they must come out, to prove
# what stays readable beside what goes.
#
# stage.replace only touches the line, so the forbidden list can't see a label.
# <fixture minus .log>.units, if there is one, holds a line per unit label:
#   <unit going in> <unit that must come out>
# Each gets a one-line log file of its own, read under that label as the journal
# source would set it.
#
# SCRUB_SHOW=1 prints what came out. Needs alloy and jq. Service repos copy
# it with the example config.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
config="${1:-${repo_root}/config/alloy/config.alloy.example}"
fixture="${2:-${repo_root}/config/alloy/testdata/scrub.log}"
forbidden="${fixture%.log}.forbidden"
caddy="${fixture%.log}.caddy.log"
expect="${fixture%.log}.expect"
units="${fixture%.log}.units"
port="${ALLOY_TEST_PORT:-12398}"

fail() { echo "scrub check: $*" >&2; exit 1; }
[[ -f "$config" && -f "$fixture" && -f "$forbidden" ]] || fail "need ${config}, ${fixture} and ${forbidden}"

work="$(mktemp -d)"
pid=
# wait: Alloy still writing as it exits would make rm fail, and with it the check.
cleanup() {
  if [[ -n "$pid" ]]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

block="$(sed -n '/^\/\/ scrub: begin/,/^\/\/ scrub: end/p' "$config")"
name="$(sed -n 's/^loki\.process "\([^"]*\)" {$/\1/p' <<<"$block" | head -1)"
[[ -n "$name" ]] || fail "no loki.process block between the scrub markers in ${config}"
[[ "$(grep -c 'forward_to' <<<"$block")" -eq 1 ]] || fail "the scrub block must have exactly one forward_to"

cp "$fixture" "${work}/in.log"
last="$(tail -n 1 "$fixture" | cut -d' ' -f1)"
[[ "$last" == keep:* || "$last" == same:* ]] || fail "the fixture's last line must be keep: or same:"
caddy_last=
total="$(wc -l <"$fixture")"
if [[ -f "$caddy" ]]; then
  cp "$caddy" "${work}/caddy.log"
  caddy_last="$(tail -n 1 "$caddy" | grep -oE 'keep:c[0-9]+' | head -1 || true)"
  [[ -n "$caddy_last" ]] || fail "the last line of $(basename "$caddy") must be a keep:c<n>"
  total=$((total + $(wc -l <"$caddy")))
fi
unit_count=0
if [[ -f "$units" ]]; then
  unit_count="$(wc -l <"$units")"
  total=$((total + unit_count))
fi
{
  printf 'logging {\n\tlevel  = "info"\n\tformat = "json"\n}\n\n'
  printf 'loki.source.file "fixture" {\n\ttargets    = [{"__path__" = "%s", "service" = "fixture", "job" = "fixture"}]\n' "${work}/in.log"
  printf '\tforward_to = [loki.process.%s.receiver]\n}\n\n' "$name"
  if [[ -n "$caddy_last" ]]; then
    printf 'loki.source.file "caddy" {\n\ttargets    = [{"__path__" = "%s", "service" = "fixture", "job" = "fixture", "container" = "caddy"}]\n' "${work}/caddy.log"
    printf '\tforward_to = [loki.process.%s.receiver]\n}\n\n' "$name"
  fi
  if [[ "$unit_count" -gt 0 ]]; then
    n=0
    while read -r unit_in _; do
      n=$((n + 1))
      echo "unit:${n}" >"${work}/unit-${n}.log"
      printf 'loki.source.file "unit_%s" {\n\ttargets    = [{"__path__" = "%s", "service" = "fixture", "job" = "fixture", "unit" = "%s"}]\n' "$n" "${work}/unit-${n}.log" "$unit_in"
      printf '\tforward_to = [loki.process.%s.receiver]\n}\n\n' "$name"
    done <"$units"
  fi
  sed -E 's/^([[:space:]]*forward_to[[:space:]]*=[[:space:]]*)\[.*\]$/\1[loki.echo.out.receiver]/' <<<"$block"
  printf '\nloki.echo "out" { }\n'
} >"${work}/harness.alloy"

alloy run --server.http.listen-addr="127.0.0.1:${port}" --storage.path="${work}/data" \
  --disable-reporting "${work}/harness.alloy" >"${work}/alloy.log" 2>&1 &
pid=$!

out="${work}/out.txt"
for _ in $(seq 60); do
  jq -r 'select(.component_id == "loki.echo.out") | .entry' "${work}/alloy.log" 2>/dev/null >"$out" || true
  grep -q "^${last} " "$out" && { [[ -z "$caddy_last" ]] || grep -qF "$caddy_last" "$out"; } &&
    [[ "$(grep -c '^unit:[0-9]*$' "$out")" -eq "$unit_count" ]] && break
  kill -0 "$pid" 2>/dev/null || { tail -20 "${work}/alloy.log" >&2; fail "alloy exited"; }
  sleep 0.5
done
grep -q "^${last} " "$out" || { tail -20 "${work}/alloy.log" >&2; fail "${last} never came out"; }
[[ -z "$caddy_last" ]] || grep -qF "$caddy_last" "$out" || { tail -20 "${work}/alloy.log" >&2; fail "${caddy_last} never came out"; }

[[ -z "${SCRUB_SHOW:-}" ]] || cat "$out"
bad=0
while read -r tag line; do
  case "$tag" in
    keep:*) grep -q "^${tag} " "$out" || { echo "missing: ${tag}"; bad=1; } ;;
    same:*) grep -qxF "${tag} ${line}" "$out" || { echo "changed: ${tag} -> $(grep "^${tag} " "$out" || echo '(missing)')"; bad=1; } ;;
    drop:*) ! grep -q "^${tag} " "$out" || { echo "shipped: ${tag}"; bad=1; } ;;
    *) fail "untagged fixture line: ${tag} ${line}" ;;
  esac
done <"$fixture"
if [[ -n "$caddy_last" ]]; then
  caddy_name="$(basename "$caddy")"
  while IFS= read -r line; do
    tag="$(grep -oE '(keep|drop):c[0-9]+' <<<"$line" | head -1 || true)"
    case "$tag" in
      keep:*) grep -qF "$tag" "$out" || { echo "missing: ${tag}"; bad=1; } ;;
      drop:*) ! grep -qF "$tag" "$out" || { echo "shipped: ${tag}"; bad=1; } ;;
      *) fail "untagged line in ${caddy_name}: ${line}" ;;
    esac
  done <"$caddy"
fi
if [[ -f "$expect" ]]; then
  while IFS= read -r line; do
    grep -qxF -- "$line" "$out" || { echo "not as expected: ${line%% *} -> $(grep "^${line%% *} " "$out" || echo '(missing)')"; bad=1; }
  done <"$expect"
fi
if [[ "$unit_count" -gt 0 ]]; then
  n=0
  while read -r unit_in unit_want; do
    n=$((n + 1))
    got="$(jq -r --arg entry "unit:${n}" 'select(.component_id == "loki.echo.out" and .entry == $entry) | .labels' "${work}/alloy.log" 2>/dev/null || true)"
    grep -qF "unit=\"${unit_want}\"" <<<"$got" || { echo "label: ${unit_in} should come out as ${unit_want}, got ${got:-(missing)}"; bad=1; }
  done <"$units"
fi
while IFS= read -r pattern; do
  [[ -z "$pattern" ]] && continue
  if hits="$(grep -E -- "$pattern" "$out")"; then
    echo "leaked /${pattern}/: ${hits//$'\n'/ | }"
    bad=1
  fi
done <"$forbidden"
[[ "$bad" -eq 0 ]] || fail "$(basename "$config") ships what it shouldn't (above)"
echo "scrub check: $(basename "$config"): $(wc -l <"$out") of ${total} lines out, all scrubbed"
