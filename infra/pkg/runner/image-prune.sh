#!/usr/bin/env bash
# image-prune: keeps the `docker` runner's Docker data root from filling up.
#
# Runs as an s6 service inside the DinD runner's container (start.sh installs
# it), so it uses the runner's own dockerd socket and its log goes out with the
# runner's (package telemetry ships container `runner`). Every line starts with
# "image-prune:".
#
# Every 30 seconds it notes which images containers use, then checks free space
# on the data root. At IMAGE_PRUNE_MIN_FREE_KIB or less it removes unused
# images, least recently used first, one at a time, until free space is above
# that line again. If removing images isn't enough, it removes build cache:
# first what's gone unused for a day, then all unused build cache.
#
# "Recently used" is the later of: the last time this loop saw a running
# container using the image, and the image's LastTagTime, which with the
# containerd image store is when this daemon last pulled, built or tagged it.
# Never removed:
#   - an image any container uses, running or stopped (`docker image rm`
#     without -f refuses those anyway);
#   - the docker label's job image (IMAGE_PRUNE_KEEP), which every job needs;
#   - while a job runs, any image used, pulled, built or tagged since the
#     earliest running container started, i.e. anything that job may still
#     want, such as an image it built and hasn't pushed yet. No job outlasts
#     the runner's timeout (IMAGE_PRUNE_JOB_MAX_SECONDS), so neither does this:
#     a container left running for longer (a `docker run -d` a job never
#     cleaned up) keeps its own image but doesn't freeze everything newer.
# Build cache a running build is using is kept by BuildKit itself.
#
# Settings (env, from infra/pkg/runner; no defaults, so Go and this can't
# drift):
#   IMAGE_PRUNE_DATA_ROOT     dockerd's data root, where free space is measured
#   IMAGE_PRUNE_MIN_FREE_KIB  prune when free space is this many KiB or less
#   IMAGE_PRUNE_KEEP          image reference never removed
#   IMAGE_PRUNE_STATE         file of "<image id> <last used, epoch seconds>"
#   IMAGE_PRUNE_JOB_MAX_SECONDS  the runner's job timeout
# For checks by hand (docs/runners.md):
#   IMAGE_PRUNE_DRY_RUN=1     list every image it would remove, in order, and
#                             the build cache steps; remove and save nothing
#   IMAGE_PRUNE_ONCE=1        one pass, then exit
#   IMAGE_PRUNE_INTERVAL      seconds between passes (default 30)
#
# A failed docker call never ends the loop: the pass is skipped and retried.
# Output it can't read (a Docker upgrade changing `image inspect`, say) is
# logged as "image-prune: ERROR: ...", so a loop that can no longer see the
# images doesn't look healthy while the disk fills.
#
# Tests: infra/pkg/runner/bats/image-prune.bats (make check).

set -uo pipefail

DATA_ROOT="${IMAGE_PRUNE_DATA_ROOT:?}"
MIN_FREE_KIB="${IMAGE_PRUNE_MIN_FREE_KIB:?}"
KEEP="${IMAGE_PRUNE_KEEP:?}"
STATE="${IMAGE_PRUNE_STATE:?}"
JOB_MAX="${IMAGE_PRUNE_JOB_MAX_SECONDS:?}"
DRY_RUN="${IMAGE_PRUNE_DRY_RUN:-0}"
ONCE="${IMAGE_PRUNE_ONCE:-0}"
INTERVAL="${IMAGE_PRUNE_INTERVAL:-30}"

# How often, at most, a status line goes out while nothing needs pruning.
STATUS_EVERY=3600
# Below the line with nothing left that may be removed (a running job holds
# the rest): build cache is retried this often, and it says so this often.
STUCK_RETRY=300
STUCK_LOG_EVERY=600

# Image ID -> epoch seconds a running container last used it. Kept in STATE.
declare -A last_used=()

# Set by observe() on every pass:
declare -A in_use=()   # image ID -> 1 if any container uses it
declare -A recency=()  # image ID -> epoch seconds it was last used or tagged
declare -A names=()    # image ID -> its names, tags and digest refs, space-separated
declare -A size_mb=()  # image ID -> its content (compressed) size in MB, for the log
keep_id=""             # IMAGE_PRUNE_KEEP's image ID, if it's present
job_started=""         # epoch seconds the earliest running container started
errors=()              # what this pass couldn't read, for report_errors
warnings=()            # what this pass found odd, for report_errors

# The readiness probe only proves dockerd answers. A Docker whose output this
# can't read would otherwise leave it pruning nothing while looking healthy, so
# that is logged as an ERROR, again this often while it lasts.
ERROR_LOG_EVERY=600

last_status=0
last_error_log=0
last_cache_prune=0
last_stuck_log=0

log() {
  echo "image-prune: $*"
}

now() {
  date +%s
}

# Free KiB on the filesystem holding the data root. Fails, printing nothing,
# if df's answer isn't a number.
free_kib() {
  local kib
  kib="$(df -Pk "$DATA_ROOT" 2>/dev/null | awk 'NR == 2 { print $4 }')"
  [[ "$kib" =~ ^[0-9]+$ ]] || return 1
  echo "$kib"
}

# "2026-11-07T02:26:05.123456789Z" -> epoch seconds, or nothing if it isn't
# one. "YYYY-MM-DD hh:mm:ss" is a form both busybox date (the runner image)
# and GNU date (the tests) read.
rfc3339_to_epoch() {
  [[ "$1" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2} ]] || return 1
  date -u -d "${1:0:10} ${1:11:8}" +%s 2>/dev/null
}

epoch_to_utc() {
  date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ
}

load_state() {
  local image seen
  [[ -f "$STATE" ]] || return 0
  while read -r image seen; do
    [[ -n "$image" && "$seen" =~ ^[0-9]+$ ]] && last_used[$image]="$seen"
  done <"$STATE"
}

# Written to a temporary file and renamed, so a restart never reads half of it.
save_state() {
  local image
  [[ "$DRY_RUN" == 1 ]] && return 0
  for image in "${!last_used[@]}"; do
    echo "$image ${last_used[$image]}"
  done >"$STATE.tmp" && mv -f "$STATE.tmp" "$STATE"
}

# Reads what the daemon has now. Returns non-zero if docker didn't answer.
observe() {
  local t containers images image running started tagged epoch size refs
  local listed=0 read_count=0 bad_dates=0 bad_starts=0 too_long=0 floor
  t="$(now)"
  # No job started before this, so no container that did is a job's.
  floor=$((t - JOB_MAX))
  in_use=()
  recency=()
  names=()
  size_mb=()
  job_started=""
  errors=()
  warnings=()

  containers="$(docker ps -aq --no-trunc)" || return 1
  if [[ -n "$containers" ]]; then
    # A container removed between `ps` and `inspect` is simply missing here.
    # shellcheck disable=SC2086 # one container ID per word
    while read -r image running started; do
      [[ -n "$image" ]] || continue
      in_use[$image]=1
      [[ "$running" == true ]] || continue
      last_used[$image]="$t"
      # A start time that doesn't parse can't say what the job touched, so it
      # counts as the earliest a job could have started: everything a job
      # could have touched is kept.
      if ! started="$(rfc3339_to_epoch "$started")" || [[ ! "$started" =~ ^[0-9]+$ ]]; then
        bad_starts=$((bad_starts + 1))
        started="$floor"
      elif ((started < floor)); then
        # Running longer than a job can: not a job's (or not one still
        # running). It keeps its own image, as in_use, and nothing more.
        too_long=$((too_long + 1))
        started="$floor"
      fi
      if [[ -z "$job_started" ]] || ((started < job_started)); then
        job_started="$started"
      fi
    done < <(docker container inspect --format '{{.Image}} {{.State.Running}} {{.State.StartedAt}}' $containers 2>/dev/null)
  fi
  if ((bad_starts > 0)); then
    errors+=("${bad_starts} running containers have a StartedAt it can't read; keeping every image touched in the last $((JOB_MAX / 3600)) hours")
  fi
  if ((too_long > 0)); then
    warnings+=("${too_long} containers have run longer than a job can ($((JOB_MAX / 3600)) hours); they keep only their own images")
  fi

  images="$(docker image ls --no-trunc --format '{{.ID}}' | sort -u)" || return 1
  if [[ -n "$images" ]]; then
    listed="$(wc -l <<<"$images")"
    # shellcheck disable=SC2086 # one image ID per word
    while read -r image tagged size refs; do
      [[ -n "$image" ]] || continue
      read_count=$((read_count + 1))
      # The CLI formats inspect's raw JSON, so LastTagTime is an RFC 3339
      # string, not a time with methods. An image never tagged has year 1:
      # it counts as now, so an image of unknown age goes last. So does one
      # whose time doesn't parse, but that one is an error.
      if [[ "$tagged" == 0001-01-01T* ]]; then
        tagged="$t"
      elif epoch="$(rfc3339_to_epoch "$tagged")" && [[ "$epoch" =~ ^[0-9]+$ ]]; then
        tagged="$epoch"
      else
        bad_dates=$((bad_dates + 1))
        tagged="$t"
      fi
      recency[$image]="$tagged"
      if [[ -n "${last_used[$image]:-}" ]] && ((last_used[$image] > tagged)); then
        recency[$image]="${last_used[$image]}"
      fi
      names[$image]="${refs//,/ }"
      size_mb[$image]=$((${size:-0} / 1000000))
    done < <(docker image inspect --format '{{.Id}} {{.Metadata.LastTagTime}} {{.Size}} {{join .RepoTags ","}}' $images 2>/dev/null)
  fi

  # Nothing read back for images that `ls` listed is what a changed inspect
  # format looks like. Fewer, but some, is an image removed between the two
  # calls, which is normal.
  if ((listed > 0 && read_count == 0)); then
    errors+=("could read 0 of ${listed} images from docker image inspect; its output format may have changed, and nothing can be pruned")
  fi
  if ((bad_dates > 0)); then
    errors+=("${bad_dates} images have a LastTagTime it can't read; counting them as used now")
  fi

  # Forget images that are gone. Not after a failed read: they're still there.
  if ((read_count > 0 || listed == 0)); then
    for image in "${!last_used[@]}"; do
      [[ -n "${recency[$image]:-}" ]] || unset 'last_used[$image]'
    done
  fi

  keep_id="$(docker image inspect --format '{{.Id}}' "$KEEP" 2>/dev/null)" || keep_id=""
  return 0
}

# Logs what this pass couldn't read (ERROR) or found odd (WARNING): on the
# first pass it happens, then every ERROR_LOG_EVERY seconds while it lasts.
report_errors() {
  local e
  ((${#errors[@]} + ${#warnings[@]} > 0)) || return 0
  if ((last_error_log == 0 || $(now) - last_error_log >= ERROR_LOG_EVERY)); then
    for e in "${errors[@]}"; do
      log "ERROR: $e"
    done
    for e in "${warnings[@]}"; do
      log "WARNING: $e"
    done
    last_error_log="$(now)"
  fi
}

# Why an image may not be removed, or nothing if it may.
protected() {
  local image="$1"
  if [[ -n "${in_use[$image]:-}" ]]; then
    echo "used by a container"
  elif [[ "$image" == "$keep_id" ]]; then
    echo "the job image"
  elif [[ -n "$job_started" ]] && ((recency[$image] >= job_started)); then
    echo "touched by the running job"
  fi
}

# Images that may be removed, least recently used first.
candidates() {
  local image
  for image in "${!recency[@]}"; do
    [[ -z "$(protected "$image")" ]] && echo "${recency[$image]} $image"
  done | sort -n | awk '{ print $2 }'
}

describe() {
  local image="$1"
  echo "${names[$image]:-<dangling>} ${image:7:12} (content ${size_mb[$image]} MB, last used $(epoch_to_utc "${recency[$image]}"))"
}

remove_image() {
  local image="$1" refs
  read -r -a refs <<<"${names[$image]}"
  # By name: an image with more than one name can't be removed by ID without
  # -f, and -f is never used. A dangling image has no name, only its ID.
  ((${#refs[@]} > 0)) || refs=("$image")
  docker image rm "${refs[@]}" >/dev/null
}

# $1: "--filter until=24h", or empty for all unused build cache.
prune_build_cache() {
  local filter="$1" what out
  what="${filter:-all unused}"
  last_cache_prune="$(now)"
  if [[ "$DRY_RUN" == 1 ]]; then
    log "would prune build cache: $what"
    return 0
  fi
  # shellcheck disable=SC2086 # the filter is empty or two words
  if out="$(docker builder prune -af $filter 2>&1)"; then
    log "pruned build cache ($what): $(tail -n 1 <<<"$out")"
  else
    log "build cache prune ($what) failed: $(tail -n 1 <<<"$out")"
  fi
}

# Re-reads free space during a prune, into the caller's $free. Without a
# number there's no knowing when to stop, so the prune stops instead.
reread_free() {
  free="$(free_kib)" && return 0
  log "ERROR: can't read free space on ${DATA_ROOT} from df; stopping this prune"
  return 1
}

prune() {
  local before free list image removed=0 why mode="" was
  reread_free || return 0
  before="$free"
  list="$(candidates)"

  # Nothing left that may be removed, and build cache was just tried: a running
  # job holds the rest. Wait for it, quietly.
  if [[ -z "$list" && "$DRY_RUN" != 1 ]] && (($(now) - last_cache_prune < STUCK_RETRY)); then
    if (($(now) - last_stuck_log >= STUCK_LOG_EVERY)); then
      log "still below line: ${free} KiB free; nothing unused to remove until the running job ends"
      last_stuck_log="$(now)"
    fi
    return 0
  fi

  [[ "$DRY_RUN" == 1 ]] && mode=" (dry run)"
  log "free ${free} KiB, line ${MIN_FREE_KIB} KiB: pruning${mode}"

  # What's kept and why, so a prune that frees too little explains itself.
  for image in "${!recency[@]}"; do
    why="$(protected "$image")"
    [[ -n "$why" ]] && log "keeping $(describe "$image"): $why"
  done

  for image in $list; do
    if [[ "$DRY_RUN" == 1 ]]; then
      log "would remove $(describe "$image")"
      continue
    fi
    ((free > MIN_FREE_KIB)) && break
    if remove_image "$image"; then
      removed=$((removed + 1))
      was="$free"
      reread_free || return 0
      log "removed $(describe "$image"): freed $((free - was)) KiB, free now ${free} KiB"
    else
      log "could not remove $(describe "$image"); skipped"
    fi
  done

  if [[ "$DRY_RUN" == 1 ]] || ((free <= MIN_FREE_KIB)); then
    prune_build_cache "--filter until=24h"
    reread_free || return 0
  fi
  if [[ "$DRY_RUN" == 1 ]] || ((free <= MIN_FREE_KIB)); then
    prune_build_cache ""
    reread_free || return 0
  fi

  log "pass done: free ${before} -> ${free} KiB (freed $((free - before)) KiB), removed ${removed} images"
  if [[ "$DRY_RUN" != 1 ]] && ((free <= MIN_FREE_KIB)); then
    log "still below line: ${free} KiB free; nothing more is safe to remove"
    last_stuck_log="$(now)"
  fi
}

# The same wait as the image's gitea-runner service, without depending on its
# s6 readiness files.
until docker info >/dev/null 2>&1; do
  sleep 2
done

load_state
log "watching ${DATA_ROOT}: prunes at ${MIN_FREE_KIB} KiB free or less; free now $(free_kib || echo '?') KiB; keeps ${KEEP}"
last_status="$(now)"

while true; do
  if observe; then
    free="$(free_kib)" || errors+=("can't read free space on ${DATA_ROOT} from df; skipping this pass")
    report_errors
    save_state
    if [[ -z "$free" ]]; then
      : # nothing to compare with the line; reported above
    elif [[ "$DRY_RUN" == 1 ]] || ((free <= MIN_FREE_KIB)); then
      prune
    elif (($(now) - last_status >= STATUS_EVERY)); then
      log "status: free ${free} KiB, ${#recency[@]} images, line ${MIN_FREE_KIB} KiB"
      last_status="$(now)"
    fi
  else
    log "docker did not answer; retrying in ${INTERVAL}s"
  fi
  [[ "$ONCE" == 1 ]] && exit 0
  sleep "$INTERVAL"
done
