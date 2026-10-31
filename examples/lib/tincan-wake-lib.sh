# shellcheck shell=sh
# Shared wake library for command-woken CLI teammates.
#
# A per-CLI wake script (run by "tincan listen --exec") sources this file and
# calls its functions in order. It is plain POSIX sh, so it works from
# /bin/sh on macOS and Linux. It is not in the release downloads: copy it
# from the repo (examples/lib/tincan-wake-lib.sh) next to the wake script
# that sources it. A per-CLI script looks like:
#
#   . "$(dirname "$0")/tincan-wake-lib.sh"
#   tincan_wake_init mycli                  # names, defaults, state dir
#   tincan_wake_begin                       # upgrade notice, lock, backoff;
#                                           # may exit 0
#   tincan_wake_check_binary "$BIN" '^MyCLI '
#   mycli_mcp_list() { ... }                # prints the CLI's MCP listing
#   tincan_wake_check_identity mycli_mcp_list   # sets tincan_wake_server
#   roots=$(tincan_wake_write_roots "$ALLOWED_ROOTS" "$WRITE_ROOTS")
#   set -- "$BIN" <headless and sandbox flags>   # plus one flag per root
#   tincan_wake_run "$@" "$PROMPT"          # timeout, failure count
#   tincan_wake_backoff "login expired"     # optional: back off at once
#
# Operator settings, read from the environment (the listener's):
#
#   TINCAN_CONFIG                  this teammate's tincan config (required)
#   TINCAN_WAKE_STATE_DIR          lock, failure count and backoff marker
#                                  (default ${TMPDIR:-/tmp}/tincan-<name>-wake)
#   TINCAN_WAKE_TIMEOUT            seconds one CLI run may take (default
#                                  1500, 25 minutes)
#   TINCAN_WAKE_PREFLIGHT_TIMEOUT  seconds the version probe and the MCP
#                                  listing may each take (default 30)
#   TINCAN_WAKE_MAX_FAILURES       failures in a row before backing off (3)
#   TINCAN_WAKE_BACKOFF            seconds to back off (default 3600, an hour)
#   TINCAN_WAKE_OPERATOR           teammate told once when the wake backs
#                                  off, or when it cannot create its state
#                                  directory (default none: no notice is
#                                  sent). The notice is sent as this
#                                  teammate, so none goes out while
#                                  TINCAN_CONFIG is unset.
#   TINCAN_WAKE_ALLOWED_SERVERS    MCP servers other than tincan the CLI may
#                                  have, comma or space separated (default
#                                  none). The CLI runs with tool approval
#                                  off, so every listed server is trusted.
#   TINCAN_BIN                     the tincan binary (default tincan)
#
# The CLI runs in a process group of its own (through perl, or setsid when
# perl is missing), so a timeout reaches everything it started, even
# processes that were reparented. The wake also saves the CLI's process
# tree every second. However the CLI ends, on time or not, whatever is left
# of its group and of the saved tree is stopped (TERM, then KILL five
# seconds later) before the lock is released, so a server the CLI started
# and left running cannot outlive the wake. With neither perl nor setsid,
# only the saved tree is stopped, so a child started and daemonized in the
# CLI's last second can escape.
#
# The lock is held from tincan_wake_begin until the script exits. A lock
# whose pid is alive is only broken once it is older than the run, the two
# preflights and one operator notice could take together, with their grace
# periods (see _tw_lock_max_minutes).
#
# Every function keeps its own variables under the _tw_ prefix, since POSIX
# sh has no local variables.

# tincan_wake_init NAME sets the defaults. NAME names the wake in log lines
# and its default state directory.
tincan_wake_init() {
  _tw_name=$1
  : "${TINCAN_WAKE_STATE_DIR:=${TMPDIR:-/tmp}/tincan-$_tw_name-wake}"
  : "${TINCAN_WAKE_TIMEOUT:=1500}"
  : "${TINCAN_WAKE_PREFLIGHT_TIMEOUT:=30}"
  : "${TINCAN_WAKE_MAX_FAILURES:=3}"
  : "${TINCAN_WAKE_BACKOFF:=3600}"
  : "${TINCAN_WAKE_OPERATOR:=}"
  : "${TINCAN_WAKE_ALLOWED_SERVERS:=}"
  : "${TINCAN_BIN:=tincan}"
  _tw_state=${TINCAN_WAKE_STATE_DIR%/}
  _tw_lock=$_tw_state/lock
  _tw_locked=
  _tw_child=
  _tw_group=
  _tw_watchdog=
  _tw_snap=
  for _tw_v in "$TINCAN_WAKE_TIMEOUT" "$TINCAN_WAKE_PREFLIGHT_TIMEOUT" "$TINCAN_WAKE_MAX_FAILURES" "$TINCAN_WAKE_BACKOFF"; do
    case $_tw_v in
      '' | *[!0-9]*)
        tincan_wake_log "TINCAN_WAKE_TIMEOUT, TINCAN_WAKE_PREFLIGHT_TIMEOUT, TINCAN_WAKE_MAX_FAILURES and TINCAN_WAKE_BACKOFF must be whole numbers"
        exit 2
        ;;
    esac
  done
}

tincan_wake_log() {
  echo "$_tw_name-wake: $*" >&2
}

# tincan_wake_upgrade_notice logs the relay's upgrade notice when "tincan
# listen" passed one in TINCAN_UPGRADE_AVAILABLE. When nothing is waiting
# (TINCAN_WAITING is 0) the nudge was for the notice alone: it exits 0
# without a CLI run, since there is nothing for one to do.
tincan_wake_upgrade_notice() {
  [ -n "${TINCAN_UPGRADE_AVAILABLE:-}" ] || return 0
  tincan_wake_log "tincan $TINCAN_UPGRADE_AVAILABLE is available from the relay: run tincan upgrade on this machine, then restart this listener"
  if [ "${TINCAN_WAITING:-}" = 0 ]; then
    tincan_wake_log "nothing is waiting, so no run"
    exit 0
  fi
}

# tincan_wake_begin passes on an upgrade notice (see
# tincan_wake_upgrade_notice), then takes the lock and checks the backoff
# marker. When a run is already in progress, or the wake is backing off, it
# exits 0 and the requests stay queued for a later nudge. When the state
# directory cannot be made, or the lock cannot be recorded in it, it tells
# the operator and exits 1.
tincan_wake_begin() {
  tincan_wake_upgrade_notice
  _tw_rc=0
  tincan_wake_lock || _tw_rc=$?
  if [ "$_tw_rc" -eq 2 ]; then
    tincan_wake_log "cannot create or secure the state directory $_tw_state (set TINCAN_WAKE_STATE_DIR to a directory this user can write); not running"
    # The notified marker cannot go in the state directory, so it goes
    # beside this teammate's config, and a successful run removes it.
    _tw_marker=${TINCAN_CONFIG:+$TINCAN_CONFIG.wake-notified}
    tincan_wake_notify "The $_tw_name wake cannot create or secure its state directory $_tw_state, so it is not running. Requests to it stay queued. Fix the directory's permissions or set TINCAN_WAKE_STATE_DIR on its machine."
    exit 1
  fi
  if [ "$_tw_rc" -eq 3 ]; then
    tincan_wake_log "cannot record the lock in $_tw_lock (is the disk holding $_tw_state full?); released it, not running"
    # The state directory may be full, so the marker goes beside the config.
    _tw_marker=${TINCAN_CONFIG:+$TINCAN_CONFIG.wake-notified}
    tincan_wake_notify "The $_tw_name wake cannot write its lock in $_tw_state, so it is not running. Requests to it stay queued. Check that the disk holding it has space and that the directory is writable, or set TINCAN_WAKE_STATE_DIR on its machine."
    exit 1
  fi
  if [ "$_tw_rc" -ne 0 ]; then
    tincan_wake_log "a run is already in progress, leaving requests queued"
    exit 0
  fi
  if tincan_wake_backoff_active; then
    tincan_wake_log "backing off after failures until $(_tw_date "$_tw_until"), leaving requests queued"
    exit 0
  fi
}

# tincan_wake_lock takes the lock directory (mkdir is atomic) and records
# in it how long this run may hold it (max, in minutes) and this script's
# pid. A lock whose pid is gone was left by a run that was killed, and is
# broken. So is one older than its holder's recorded bound, whatever its pid
# says, since pids are reused. The holder's bound is used rather than this
# run's, so a wake configured with shorter timeouts does not break a longer
# run's lock early; only a lock without one (left by an older library) falls
# back to this run's own (_tw_lock_max_minutes). Returns 1 when a live run
# holds it, 2 when the state directory cannot be made, and 3 when the bound
# or the pid cannot be written: the lock is then released rather than held
# unrecorded, since a later wake could break it early or find no pid.
tincan_wake_lock() {
  { mkdir -p "$_tw_state" && chmod 700 "$_tw_state"; } 2>/dev/null || return 2
  trap '_tw_cleanup' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if ! mkdir "$_tw_lock" 2>/dev/null; then
    _tw_pid=$(cat "$_tw_lock/pid" 2>/dev/null) || _tw_pid=
    _tw_max=$(cat "$_tw_lock/max" 2>/dev/null) || _tw_max=
    case $_tw_max in '' | *[!0-9]*) _tw_max=$(_tw_lock_max_minutes) ;; esac
    if [ -n "$_tw_pid" ] && kill -0 "$_tw_pid" 2>/dev/null && ! _tw_older "$_tw_lock" "$_tw_max"; then
      return 1
    fi
    # No pid yet: another wake made the lock a moment ago.
    if [ -z "$_tw_pid" ] && ! _tw_older "$_tw_lock" 1; then
      return 1
    fi
    # Move the stale lock aside under a name only this run uses, and put it
    # back if another wake replaced it in the meantime.
    _tw_stale=$_tw_lock.stale.$$
    mv "$_tw_lock" "$_tw_stale" 2>/dev/null || return 1
    if [ "$(cat "$_tw_stale/pid" 2>/dev/null)" != "$_tw_pid" ]; then
      mv "$_tw_stale" "$_tw_lock" 2>/dev/null || rm -rf "$_tw_stale"
      return 1
    fi
    rm -rf "$_tw_stale"
    tincan_wake_log "broke a stale lock left by pid ${_tw_pid:-unknown}"
    mkdir "$_tw_lock" 2>/dev/null || return 1
  fi
  # The bound goes in before the pid, so a lock with a pid has its bound.
  if ! { _tw_lock_max_minutes >"$_tw_lock/max" && echo $$ >"$_tw_lock/pid"; } 2>/dev/null; then
    rm -rf "$_tw_lock"
    return 3
  fi
  _tw_locked=1
}

# _tw_lock_max_minutes prints how many minutes a live wake may hold the
# lock: the CLI run, the version probe, the MCP listing and one operator
# notice, each with the time it takes to stop it and whatever it left
# running (a second tick, five seconds from TERM to KILL, five more to reap
# leftovers, with room to spare: fifteen seconds), rounded up, plus five
# minutes. A lock older than this is broken even if its pid is alive.
_tw_lock_max_minutes() {
  _tw_secs=$((TINCAN_WAKE_TIMEOUT + 3 * TINCAN_WAKE_PREFLIGHT_TIMEOUT + 4 * 15))
  echo $(((_tw_secs + 59) / 60 + 5))
}

# _tw_older DIR MINUTES succeeds if DIR was last changed more than MINUTES
# minutes ago.
_tw_older() {
  [ -n "$(find "$1" -maxdepth 0 -mmin +"$2" 2>/dev/null)" ]
}

_tw_cleanup() {
  if [ -n "$_tw_watchdog" ]; then
    kill "$_tw_watchdog" 2>/dev/null || true
  fi
  if [ -n "$_tw_child" ]; then
    _tw_signal KILL "$_tw_group" "$(_tw_tree "$_tw_child") $(_tw_saved_tree)"
    rm -f "$_tw_snap" "$_tw_snap.tmp"
  fi
  if [ -n "$_tw_locked" ] && [ "$(cat "$_tw_lock/pid" 2>/dev/null)" = "$$" ]; then
    rm -rf "$_tw_lock"
  fi
}

_tw_now() {
  date +%s
}

_tw_date() {
  date -r "$1" 2>/dev/null || date -d "@$1" 2>/dev/null || echo "epoch $1"
}

# tincan_wake_backoff_active succeeds while the backoff marker is in force,
# leaving its end time in _tw_until. A marker that has run out is removed;
# the failure count stays, so one more failure backs off again.
tincan_wake_backoff_active() {
  _tw_until=$(cat "$_tw_state/backoff" 2>/dev/null) || return 1
  case $_tw_until in '' | *[!0-9]*) _tw_until=0 ;; esac
  if [ "$(_tw_now)" -lt "$_tw_until" ]; then
    return 0
  fi
  rm -f "$_tw_state/backoff"
  return 1
}

# _tw_backoff REASON sets the backoff marker and tells the operator.
_tw_backoff() {
  _tw_until=$(($(_tw_now) + TINCAN_WAKE_BACKOFF))
  echo "$_tw_until" >"$_tw_state/backoff"
  tincan_wake_log "backing off until $(_tw_date "$_tw_until"): $1"
  tincan_wake_notify "The $_tw_name wake stopped running and is backing off until $(_tw_date "$_tw_until"): $1. Requests to it stay queued. Check the wake's log on its machine; one successful run clears this."
}

# tincan_wake_notify MESSAGE sends MESSAGE to TINCAN_WAKE_OPERATOR, as this
# teammate, once until a run succeeds (the notified marker is _tw_marker,
# default in the state directory). Nothing is sent when no operator is set,
# or when TINCAN_CONFIG is unset, since tincan would then send it as
# whichever agent its default config names. A failed send is only logged.
tincan_wake_notify() {
  [ -n "$TINCAN_WAKE_OPERATOR" ] || return 0
  if [ -z "${TINCAN_CONFIG:-}" ]; then
    tincan_wake_log "not notifying $TINCAN_WAKE_OPERATOR: TINCAN_CONFIG is not set, so the notice would not come from this teammate"
    return 0
  fi
  _tw_marker=${_tw_marker:-$_tw_state/notified}
  [ ! -e "$_tw_marker" ] || return 0
  # Supervised like a preflight, so a hung send cannot hold the lock.
  if _tw_supervise "$TINCAN_WAKE_PREFLIGHT_TIMEOUT" "$TINCAN_BIN" ask --notify "$TINCAN_WAKE_OPERATOR" "$1" >/dev/null 2>&1; then
    : >"$_tw_marker" 2>/dev/null || true
  else
    tincan_wake_log "could not notify $TINCAN_WAKE_OPERATOR"
  fi
}

# tincan_wake_refuse REASON stops this wake before the CLI runs: it backs
# off, tells the operator, and exits 1. For problems a retry cannot fix.
tincan_wake_refuse() {
  tincan_wake_log "refusing to run: $1"
  _tw_backoff "$1"
  exit 1
}

# tincan_wake_backoff REASON backs off at once and tells the operator, after
# a run failed in a way the next run cannot fix on its own (an expired
# login). The requests stay queued.
tincan_wake_backoff() {
  _tw_backoff "$1"
}

# tincan_wake_record_failure REASON counts one failed run, and backs off
# once TINCAN_WAKE_MAX_FAILURES runs in a row have failed.
tincan_wake_record_failure() {
  _tw_n=$(cat "$_tw_state/failures" 2>/dev/null) || _tw_n=0
  case $_tw_n in '' | *[!0-9]*) _tw_n=0 ;; esac
  _tw_n=$((_tw_n + 1))
  echo "$_tw_n" >"$_tw_state/failures"
  tincan_wake_log "run failed ($1), $_tw_n in a row"
  if [ "$_tw_n" -ge "$TINCAN_WAKE_MAX_FAILURES" ]; then
    _tw_backoff "$_tw_n runs in a row failed, the last one: $1"
  fi
}

# tincan_wake_record_success clears the failure count, the backoff marker
# and the notice markers.
tincan_wake_record_success() {
  rm -f "$_tw_state/failures" "$_tw_state/backoff" "$_tw_state/notified"
  if [ -n "${TINCAN_CONFIG:-}" ]; then
    rm -f "$TINCAN_CONFIG.wake-notified"
  fi
}

# _tw_preflight OUTFILE CMD [ARGS...] runs CMD (a program or a shell
# function) with its output in OUTFILE, under TINCAN_WAKE_PREFLIGHT_TIMEOUT.
# Returns CMD's status, or 124 when it timed out.
_tw_preflight() {
  _tw_pf_out=$1
  shift
  _tw_supervise "$TINCAN_WAKE_PREFLIGHT_TIMEOUT" "$@" >"$_tw_pf_out" 2>&1
}

# tincan_wake_check_binary BIN PATTERN [ARGS...] runs BIN ARGS (default
# --version) and refuses to wake unless its output matches the extended
# regular expression PATTERN, so a different tool of the same name never
# runs with this teammate's tools.
tincan_wake_check_binary() {
  _tw_bin=$1
  _tw_pattern=$2
  shift 2
  [ $# -gt 0 ] || set -- --version
  if ! command -v "$_tw_bin" >/dev/null 2>&1; then
    tincan_wake_refuse "$_tw_bin is not installed or not on PATH"
  fi
  _tw_rc=0
  _tw_preflight "$_tw_state/preflight.$$" "$_tw_bin" "$@" || _tw_rc=$?
  _tw_out=$(cat "$_tw_state/preflight.$$" 2>/dev/null) || _tw_out=
  rm -f "$_tw_state/preflight.$$"
  if [ "$_tw_rc" -eq 124 ]; then
    tincan_wake_refuse "$_tw_bin $* timed out after $TINCAN_WAKE_PREFLIGHT_TIMEOUT seconds"
  fi
  if [ "$_tw_rc" -ne 0 ]; then
    tincan_wake_refuse "$_tw_bin $* failed: $(printf '%s\n' "$_tw_out" | head -n 1)"
  fi
  if ! printf '%s\n' "$_tw_out" | grep -Eq -- "$_tw_pattern"; then
    tincan_wake_refuse "$_tw_bin is not the expected tool: $_tw_bin $* says \"$(printf '%s\n' "$_tw_out" | head -n 1)\""
  fi
}

# tincan_wake_check_identity LISTFN pins this wake to its own teammate. The
# per-CLI function LISTFN prints the CLI's MCP servers, one per line, as
#
#   name<TAB>TINCAN_CONFIG from the server's env (empty if none)<TAB>command and args
#
# Blank lines are skipped; a row with no name stops the wake. A server is
# agent-tincan when its name contains tincan (any case) or its command's
# executable is named tincan; a server that only mentions tincan in its
# arguments (say a filesystem server on ~/code/agent-tincan) is an ordinary
# server. The listing runs under TINCAN_WAKE_PREFLIGHT_TIMEOUT.
# The wake refuses to run unless there is exactly one, its TINCAN_CONFIG is
# this wake's TINCAN_CONFIG, and every other server is in
# TINCAN_WAKE_ALLOWED_SERVERS: the CLI runs with tool approval off, so it
# must not be able to act as another teammate or through an unvetted server.
# When the check passes, tincan_wake_server holds that server's name (for a
# CLI flag that names the servers it may start).
tincan_wake_check_identity() {
  if [ -z "${TINCAN_CONFIG:-}" ]; then
    tincan_wake_refuse "TINCAN_CONFIG is not set for this wake"
  fi
  _tw_rc=0
  _tw_preflight "$_tw_state/listing.$$" "$1" || _tw_rc=$?
  _tw_listing=$(cat "$_tw_state/listing.$$" 2>/dev/null) || _tw_listing=
  rm -f "$_tw_state/listing.$$"
  if [ "$_tw_rc" -eq 124 ]; then
    tincan_wake_refuse "listing the CLI's MCP servers timed out after $TINCAN_WAKE_PREFLIGHT_TIMEOUT seconds"
  fi
  if [ "$_tw_rc" -ne 0 ]; then
    tincan_wake_refuse "could not list the CLI's MCP servers"
  fi
  _tw_allowed=$(printf '%s' "$TINCAN_WAKE_ALLOWED_SERVERS" | tr ',' ' ')
  # awk -F '\t' keeps an empty TINCAN_CONFIG field in place, where the
  # shell's read would collapse the two tabs around it. tincan() looks at
  # the name and the command's executable, never its other arguments.
  # shellcheck disable=SC2016 # awk program text, expanded by awk
  _tw_awk_lib='
    function tincan(  argv, exe) {
      if (tolower($1) ~ /tincan/) return 1
      split($3, argv, " ")
      exe = argv[1]
      sub(/.*\//, "", exe)
      return exe == "tincan"
    }'
  _tw_noname=$(printf '%s\n' "$_tw_listing" | awk -F '\t' '$0 != "" && $1 !~ /[^ ]/ { n++ } END { print n + 0 }')
  if [ "$_tw_noname" -ne 0 ]; then
    tincan_wake_refuse "the CLI listed an MCP server with no name"
  fi
  _tw_count=$(printf '%s\n' "$_tw_listing" | awk -F '\t' "$_tw_awk_lib"' $0 != "" && tincan() { n++ } END { print n + 0 }')
  _tw_tincan=$(printf '%s\n' "$_tw_listing" | awk -F '\t' "$_tw_awk_lib"' $0 != "" && tincan() { print $2; exit }')
  _tw_tincan_name=$(printf '%s\n' "$_tw_listing" | awk -F '\t' "$_tw_awk_lib"' $0 != "" && tincan() { print $1; exit }')
  _tw_extra=$(printf '%s\n' "$_tw_listing" | awk -F '\t' -v allowed="$_tw_allowed" "$_tw_awk_lib"'
    BEGIN { n = split(allowed, a, " "); for (i = 1; i <= n; i++) ok[a[i]] = 1 }
    $0 == "" || tincan() { next }
    !($1 in ok) { printf "%s%s", sep, $1; sep = ", " }')
  if [ "$_tw_count" -ne 1 ]; then
    tincan_wake_refuse "the CLI has $_tw_count agent-tincan servers; it needs exactly one, with TINCAN_CONFIG=$TINCAN_CONFIG"
  fi
  if [ -z "$_tw_tincan" ]; then
    tincan_wake_refuse "the CLI's agent-tincan server sets no TINCAN_CONFIG; set it to $TINCAN_CONFIG"
  fi
  if ! _tw_same_file "$_tw_tincan" "$TINCAN_CONFIG"; then
    tincan_wake_refuse "the CLI's agent-tincan server uses TINCAN_CONFIG=$_tw_tincan, not this wake's $TINCAN_CONFIG"
  fi
  if [ -n "$_tw_extra" ]; then
    tincan_wake_refuse "the CLI has MCP servers the operator did not allow: $_tw_extra (list them in TINCAN_WAKE_ALLOWED_SERVERS, or remove them)"
  fi
  # shellcheck disable=SC2034 # read by the per-CLI script
  tincan_wake_server=$_tw_tincan_name
}

# _tw_same_file A B succeeds if A and B are the same path, or name the same
# file once their directories are resolved.
_tw_same_file() {
  [ "$1" = "$2" ] && return 0
  _tw_a=$(tincan_wake_canonical_dir "$(dirname -- "$1")") || return 1
  _tw_b=$(tincan_wake_canonical_dir "$(dirname -- "$2")") || return 1
  [ "$_tw_a/$(basename -- "$1")" = "$_tw_b/$(basename -- "$2")" ]
}

# tincan_wake_canonical_dir prints the physical path of an existing
# directory given by absolute path, or fails. macOS has no realpath -m, so
# it resolves with cd -P in a subshell.
tincan_wake_canonical_dir() {
  case "$1" in
    /*) ;;
    *) return 1 ;;
  esac
  (CDPATH='' cd -P -- "$1" 2>/dev/null && pwd -P)
}

# tincan_wake_under_root CANON ROOTS succeeds if canonical path CANON is at
# or under one of ROOTS (colon-separated, each canonicalized).
tincan_wake_under_root() {
  _tw_canon=$1
  _tw_old_ifs=$IFS
  IFS=:
  set -f
  # shellcheck disable=SC2086 # split on IFS=: on purpose
  set -- $2
  set +f
  IFS=$_tw_old_ifs
  for _tw_root in "$@"; do
    [ -n "$_tw_root" ] || continue
    _tw_root_canon=$(tincan_wake_canonical_dir "$_tw_root") || continue
    [ "$_tw_root_canon" = / ] && _tw_root_canon=
    case "$_tw_canon/" in
      "$_tw_root_canon"/*) return 0 ;;
    esac
  done
  return 1
}

# tincan_wake_write_roots ALLOWED WRITE prints, one per line, the canonical
# path of each entry of WRITE (colon-separated absolute paths the operator
# opened) that is at or under a root in ALLOWED (colon-separated). Anything
# else, a relative or missing path, a path outside every allowed root, a ../
# or symlink that climbs out of one, is skipped with a note on stderr. The
# per-CLI script turns each line into its CLI's write-root flag; nothing
# the model says can add one.
tincan_wake_write_roots() {
  _tw_allowed_roots=$1
  _tw_old_ifs=$IFS
  IFS=:
  set -f
  # shellcheck disable=SC2086 # split on IFS=: on purpose
  set -- $2
  set +f
  IFS=$_tw_old_ifs
  for _tw_w in "$@"; do
    [ -n "$_tw_w" ] || continue
    if ! _tw_wc=$(tincan_wake_canonical_dir "$_tw_w"); then
      tincan_wake_log "skipping write root $_tw_w: not an absolute path to an existing directory"
      continue
    fi
    case $_tw_wc in
      *'
'*)
        tincan_wake_log "skipping write root $_tw_w: its path has a newline"
        continue
        ;;
    esac
    if tincan_wake_under_root "$_tw_wc" "$_tw_allowed_roots"; then
      printf '%s\n' "$_tw_wc"
    else
      tincan_wake_log "refusing write root $_tw_w (resolves to $_tw_wc): not under an allowed root ($_tw_allowed_roots)"
    fi
  done
}

# _tw_tree PID prints PID and all its descendants.
_tw_tree() {
  _tw_queue=$1
  _tw_all=
  while :; do
    # shellcheck disable=SC2086
    set -- $_tw_queue
    [ $# -gt 0 ] || break
    _tw_p=$1
    shift
    _tw_queue=$*
    _tw_all="$_tw_all $_tw_p"
    if command -v pgrep >/dev/null 2>&1; then
      _tw_queue="$_tw_queue $(pgrep -P "$_tw_p" 2>/dev/null | tr '\n' ' ')"
    fi
  done
  # shellcheck disable=SC2086
  echo $_tw_all
}

# _tw_signal SIGNAL GROUP PIDS sends SIGNAL to process group GROUP (if set)
# and to each of PIDS.
_tw_signal() {
  if [ -n "$2" ]; then
    kill -"$1" -"$2" 2>/dev/null || true
  fi
  # shellcheck disable=SC2086 # a list of pids
  [ -z "$3" ] || kill -"$1" $3 2>/dev/null || true
}

# _tw_any_alive GROUP PIDS succeeds while the group or any of PIDS lives.
_tw_any_alive() {
  if [ -n "$1" ] && kill -0 -"$1" 2>/dev/null; then
    return 0
  fi
  for _tw_p in $2; do
    kill -0 "$_tw_p" 2>/dev/null && return 0
  done
  return 1
}

# _tw_saved_tree prints the pids in the last saved process tree of the CLI,
# without the CLI itself.
_tw_saved_tree() {
  [ -n "$_tw_snap" ] || return 0
  # The saved tree is one line of pids.
  _tw_line=
  { read -r _tw_line <"$_tw_snap"; } 2>/dev/null || :
  for _tw_p in $_tw_line; do
    [ "$_tw_p" = "$_tw_child" ] || printf '%s ' "$_tw_p"
  done
}

# _tw_reap GROUP PIDS stops what is left of process group GROUP and of PIDS:
# TERM, then KILL five seconds later to whatever ignored it.
_tw_reap() {
  _tw_any_alive "$1" "$2" || return 0
  tincan_wake_log "stopping processes the CLI left running"
  _tw_signal TERM "$1" "$2"
  _tw_i=0
  while [ "$_tw_i" -lt 5 ] && _tw_any_alive "$1" "$2"; do
    sleep 1
    _tw_i=$((_tw_i + 1))
  done
  _tw_signal KILL "$1" "$2"
}

# _tw_pgrp_tool prints how to start a program in a process group of its
# own, keeping its pid: perl, else setsid (which keeps the pid as long as
# the caller is not a group leader, and a background job without job
# control never is), else nothing. _TW_NO_PGRP forces the fallback, for the
# tests.
_tw_pgrp_tool() {
  [ -z "${_TW_NO_PGRP:-}" ] || return 0
  if command -v perl >/dev/null 2>&1; then
    echo perl
  elif command -v setsid >/dev/null 2>&1; then
    echo setsid
  fi
}

# _tw_supervise TIMEOUT CMD [ARGS...] runs CMD with stdin closed and a hard
# TIMEOUT in seconds (GNU timeout is not on macOS, so a watchdog does it).
# A program runs in a process group of its own when setsid or perl can make
# one; a shell function runs in a subshell. The watchdog saves the process
# tree every second. On timeout it sends TERM to the group and the tree,
# and five seconds later KILL to whatever is left, so descendants that
# ignore TERM or were reparented still die. When CMD exits on its own,
# whatever is left of its group and saved tree is stopped the same way, so
# nothing it started outlives it. Returns CMD's status, or 124 when it
# timed out.
_tw_supervise() {
  _tw_limit=$1
  shift
  _tw_timed_out=$_tw_state/timed-out.$$
  _tw_snap=$_tw_state/tree.$$
  rm -f "$_tw_timed_out" "$_tw_snap"
  _tw_tool=
  case $(command -v "$1" 2>/dev/null) in
    */*) _tw_tool=$(_tw_pgrp_tool) ;;
  esac
  case $_tw_tool in
    setsid) setsid "$@" </dev/null & ;;
    perl) perl -e 'setpgrp(0, 0); exec { $ARGV[0] } @ARGV; print STDERR "$ARGV[0]: $!\n"; exit 127' -- "$@" </dev/null & ;;
    *) "$@" </dev/null & ;;
  esac
  _tw_child=$!
  # setsid and perl keep the pid, so the group's id is the child's pid.
  _tw_group=
  [ -z "$_tw_tool" ] || _tw_group=$_tw_child
  (
    trap - EXIT HUP INT TERM
    _tw_i=0
    while [ "$_tw_i" -lt "$_tw_limit" ]; do
      sleep 1
      kill -0 "$_tw_child" 2>/dev/null || exit 0
      # Saved for the reap after the CLI exits, when its children have
      # been reparented and can no longer be found from its pid. The
      # rename keeps a reader from seeing half a list; a state directory
      # that cannot be written only loses the list.
      { _tw_tree "$_tw_child" >"$_tw_snap.tmp" && mv -f "$_tw_snap.tmp" "$_tw_snap"; } 2>/dev/null || :
      _tw_i=$((_tw_i + 1))
    done
    : >"$_tw_timed_out" 2>/dev/null || :
    _tw_saved="$(_tw_tree "$_tw_child") $(_tw_saved_tree)"
    _tw_signal TERM "$_tw_group" "$_tw_saved"
    _tw_i=0
    while [ "$_tw_i" -lt 5 ] && _tw_any_alive "$_tw_group" "$_tw_saved"; do
      sleep 1
      _tw_i=$((_tw_i + 1))
    done
    _tw_signal KILL "$_tw_group" "$_tw_saved"
  ) </dev/null >/dev/null 2>&1 &
  _tw_watchdog=$!
  _tw_status=0
  wait "$_tw_child" || _tw_status=$?
  if [ -e "$_tw_timed_out" ]; then
    # The watchdog is between TERM and KILL: let it finish.
    wait "$_tw_watchdog" 2>/dev/null || true
  else
    kill "$_tw_watchdog" 2>/dev/null || true
    wait "$_tw_watchdog" 2>/dev/null || true
    # It may have fired between the wait and the kill.
    if [ -e "$_tw_timed_out" ]; then
      _tw_signal KILL "$_tw_group" ""
    fi
  fi
  _tw_watchdog=
  _tw_reap "$_tw_group" "$(_tw_saved_tree)"
  rm -f "$_tw_snap" "$_tw_snap.tmp"
  _tw_child=
  _tw_group=
  if [ -e "$_tw_timed_out" ]; then
    rm -f "$_tw_timed_out"
    return 124
  fi
  return "$_tw_status"
}

# tincan_wake_run CMD [ARGS...] runs the CLI under TINCAN_WAKE_TIMEOUT (see
# _tw_supervise). A clean exit clears the failure count; anything else
# counts as a failure. Returns the CLI's status, or 124 when it timed out.
tincan_wake_run() {
  _tw_run_status=0
  _tw_supervise "$TINCAN_WAKE_TIMEOUT" "$@" || _tw_run_status=$?
  if [ "$_tw_run_status" -eq 124 ]; then
    tincan_wake_record_failure "timed out after $TINCAN_WAKE_TIMEOUT seconds"
    return 124
  fi
  if [ "$_tw_run_status" -eq 0 ]; then
    tincan_wake_record_success
    return 0
  fi
  tincan_wake_record_failure "exit status $_tw_run_status"
  return "$_tw_run_status"
}

# tincan_wake_uuid prints a new lowercase UUID (uuidgen,
# /proc/sys/kernel/random/uuid or python3), or fails.
tincan_wake_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    _tw_uuid=$(uuidgen | tr 'A-F' 'a-f')
  elif [ -r /proc/sys/kernel/random/uuid ]; then
    _tw_uuid=$(cat /proc/sys/kernel/random/uuid)
  elif command -v python3 >/dev/null 2>&1; then
    _tw_uuid=$(python3 -c 'import uuid; print(uuid.uuid4())')
  else
    return 1
  fi
  printf '%s\n' "$_tw_uuid" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || return 1
  printf '%s\n' "$_tw_uuid"
}

# tincan_wake_record_session LIST WORKDIR [ID] records a wake run in LIST,
# the private <name>.wake-sessions file beside the teammate's config that
# the history agent reads to leave wake runs out: a "workdir <path>" line
# once, then ID on a line of its own when one is given (letters, digits,
# dot, dash and underscore only; anything else is skipped). The file is
# made mode 600. A list that cannot be written is only logged.
tincan_wake_record_session() {
  if ! { (umask 077 && touch "$1") && chmod 600 "$1"; } 2>/dev/null; then
    tincan_wake_log "cannot record this run in $1; the history agent may list it as the owner's"
    return 0
  fi
  grep -qxF "workdir $2" "$1" 2>/dev/null || printf 'workdir %s\n' "$2" >>"$1"
  [ -n "${3:-}" ] || return 0
  if printf '%s\n' "$3" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'; then
    printf '%s\n' "$3" >>"$1"
  else
    tincan_wake_log "not recording the session id \"$3\": not a plain id"
  fi
}

# tincan_wake_json_string prints $1 as a JSON string, quotes and
# backslashes escaped. For paths, which hold no control characters.
tincan_wake_json_string() {
  printf '"%s"' "$(printf '%s' "$1" | sed 's/[\\"]/\\&/g')"
}

# tincan_wake_mcp_json TOOL FILE... prints the mcpServers of each JSON
# config FILE that exists as tincan_wake_check_identity's listing lines:
# name, TINCAN_CONFIG, command and args. TOOL is jq or python3.
tincan_wake_mcp_json() {
  _tw_tool=$1
  shift
  for _tw_f in "$@"; do
    [ -e "$_tw_f" ] || continue
    case $_tw_tool in
      jq)
        jq -r '(.mcpServers // {}) | to_entries[]
          | [.key,
             ((.value.env // {}).TINCAN_CONFIG // "" | tostring),
             ([.value.command // .value.url // ""]
               + ((.value.args // []) | map(tostring)) | join(" "))]
          | map(gsub("[\t\n]"; " ")) | join("\t")' "$_tw_f" || return 1
        ;;
      python3)
        python3 -c '
import json, sys
servers = json.load(open(sys.argv[1])).get("mcpServers") or {}
for name, v in servers.items():
    env = v.get("env") or {}
    cmd = [v.get("command") or v.get("url") or ""]
    cmd += [str(a) for a in (v.get("args") or [])]
    row = [name, str(env.get("TINCAN_CONFIG") or ""), " ".join(cmd)]
    print("\t".join(x.replace("\t", " ").replace("\n", " ") for x in row))
' "$_tw_f" || return 1
        ;;
      *)
        tincan_wake_log "reading $_tw_f needs jq or python3 on PATH"
        return 1
        ;;
    esac
  done
}

# tincan_wake_json_tool prints the JSON reader to use: $1 when set (jq or
# python3), else jq when on PATH, else python3; nothing when that tool is
# missing.
tincan_wake_json_tool() {
  _tw_jt=${1:-}
  if [ -z "$_tw_jt" ]; then
    if command -v jq >/dev/null 2>&1; then _tw_jt=jq; else _tw_jt=python3; fi
  fi
  case $_tw_jt in
    jq | python3) command -v "$_tw_jt" >/dev/null 2>&1 && echo "$_tw_jt" ;;
  esac
  return 0
}

# tincan_wake_tools lists the Agent Tincan tools (internal/mcpserver), the
# only ones a wake that allowlists tools lets a run use.
# shellcheck disable=SC2034 # read by the wake scripts
tincan_wake_tools="ask get_reply check_inbox claim progress reply answer cancel list_agents onboard get_attachment trace search watch list_rooms room_read room_post lease list_leases list_schedules cancel_schedule"

# tincan_wake_config_path CONFIG prints CONFIG with a leading ~/ expanded
# (as tincan does) and made absolute against the current directory, for a
# wake that starts its run from another directory.
tincan_wake_config_path() {
  _tw_cp=$1
  # shellcheck disable=SC2088 # a literal ~/ prefix, which tincan expands
  case $_tw_cp in "~/"*) _tw_cp=$HOME/${_tw_cp#"~/"} ;; esac
  case $_tw_cp in /*) ;; *) _tw_cp=$(pwd -P)/$_tw_cp ;; esac
  printf '%s\n' "$_tw_cp"
}

# tincan_wake_beside_config CONFIG SUFFIX prints <dir>/<name>SUFFIX, where
# <dir> is CONFIG's directory and <name> its file name without .json: the
# wake's files kept beside the teammate's config (.wake-sessions, the list
# tincan_wake_record_session writes; .wake, a wake home; .wake-state).
tincan_wake_beside_config() {
  printf '%s\n' "$(dirname -- "$1")/$(basename -- "$1" .json)$2"
}

# tincan_wake_resolve_tincan sets tincan_wake_tincan to the absolute path of
# the tincan binary (TINCAN_BIN, else tincan, on PATH), for an MCP config
# the wake writes. It refuses to wake when tincan is missing, or when the
# name is not a program (a shell function or alias, say).
tincan_wake_resolve_tincan() {
  tincan_wake_tincan=$(command -v "${TINCAN_BIN:-tincan}" 2>/dev/null) || tincan_wake_refuse "${TINCAN_BIN:-tincan} is not installed or not on PATH"
  case $tincan_wake_tincan in /*) ;; *) tincan_wake_refuse "${TINCAN_BIN:-tincan} is not a program on PATH; set TINCAN_BIN to the tincan binary's path" ;; esac
}

# tincan_wake_write_file FILE writes stdin to FILE through a temporary file
# beside it, and refuses to wake when it cannot.
tincan_wake_write_file() {
  if ! { cat >"$1.$$" && mv -f "$1.$$" "$1"; } 2>/dev/null; then
    rm -f "$1.$$"
    tincan_wake_refuse "cannot write $1"
  fi
}
