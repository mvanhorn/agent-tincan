#!/bin/bash
# examples/grokbot/tincan-up.sh: bring Tailscale (and optionally the Tincan relay) back after the host
# was wiped or moved. Idempotent; safe to run every hour. Runs as the normal user, no sudo.
#
# See docs/adapters/grokbot.md. Hosts that keep only one folder other than the home folder
# (Hark keeps /workspace) set TAILSCALE_LIB, TAILSCALE_BIN, TAILSCALE_STATEDIR, TAILSCALE_CACHE
# and TS_SOCKET: see docs/adapters/hark.md. Unset, every path is the Grok Bot layout below.
# Exit codes: 0 healthy | 1 unhealthy | 2 device needs approval in the Tailscale admin
#             3 auth key missing, wrong type, expired, used or rejected
#             4 legacy layout: a relay with --listen or a system tailscaled is running
set -euo pipefail

TS_HOSTNAME="${TS_HOSTNAME:-grokbot}"        # the machine name the relay knows
TS_TAGS="${TS_TAGS:-tag:grokbot}"            # must match the tag on the auth key
TS_VERSION="${TS_VERSION:-}"                 # pin a version, or empty for latest stable
PROXY_ADDR="${PROXY_ADDR:-localhost:1055}"   # SOCKS5 + HTTP proxy into the tailnet
START_RELAY="${START_RELAY:-0}"              # 1 if this host also runs `tincan relay` (see the doc first)
RELAY_ADMIN="${RELAY_ADMIN:-}"               # --admin list for that relay, e.g. my-laptop
# RELAY_TS_AUTHKEY: the relay's own one-off auth key, used only while the relay has no saved identity

# Where things live. The defaults are the Grok Bot layout, where the home folder survives a wipe.
# A host that keeps only another folder (Hark keeps /workspace) points the binaries, the node
# identity and the socket there, and keeps the cache (lock, log) on local disk.
LIB="${TAILSCALE_LIB:-$HOME/.local/lib/tailscale}"       # real binaries
BIN="${TAILSCALE_BIN:-$HOME/.local/bin}"                 # wrapper `tailscale` (on PATH)
STATEDIR="${TAILSCALE_STATEDIR:-$HOME/.config/tailscale}" # node identity: must survive a wipe
CACHE="${TAILSCALE_CACHE:-$HOME/.cache/tailscale}"       # lock and log: local disk
SOCK="${TS_SOCKET:-$CACHE/tailscaled.sock}"              # the same TS_SOCKET tincan reads
LOG="$CACHE/tailscaled.log"
TINCAN="${TINCAN:-$BIN/tincan}"

mkdir -p "$LIB" "$BIN" "$STATEDIR" "$CACHE" "$(dirname "$SOCK")"
chmod 700 "$STATEDIR" "$CACHE"

exec 9>"$CACHE/tincan-up.lock"
if command -v flock >/dev/null && ! flock -n 9; then echo "already running"; exit 0; fi

# 0. Refuse to run next to the old layout: a relay bound to the system Tailscale with --listen, or
#    a system tailscaled keeping its state in /var/lib/tailscale (its default). Starting a second
#    tailscaled or relay beside them would fight over the node and the relay's database.
#    A tailscaled serving this script's own socket is the daemon this host uses, so it is
#    adopted as it is, whatever its state flags; the --listen relay is refused regardless.
legacy=$(ps -eo args= 2>/dev/null | awk -v sock="$SOCK" '
  { n = split($1, p, "/"); b = p[n] }
  b == "tincan" && $2 == "relay" {
    for (i = 3; i <= NF; i++) if ($i ~ /^--?listen(=|$)/) { print "a tincan relay started with --listen"; exit }
  }
  b == "tailscaled" {
    for (i = 2; i <= NF; i++) if ($i == "--socket=" sock || $i == "-socket=" sock || (($i == "--socket" || $i == "-socket") && $(i + 1) == sock)) next
    s = 0; for (i = 2; i <= NF; i++) if ($i ~ /^--?(state|statedir)(=|$)/) s = 1
    if (!s || index($0, "/var/lib/tailscale")) { print "a system tailscaled with its state in /var/lib/tailscale"; exit }
  }' || true)
if [ -n "$legacy" ]; then
  echo "legacy layout: $legacy is running. Stop it and move this box to the layout in docs/adapters/grokbot.md; nothing was started"
  exit 4
fi

tsc() { "$LIB/tailscale" --socket="$SOCK" "$@"; }
backend() { { tsc status --json 2>/dev/null || true; } | sed -n 's/.*"BackendState": *"\([^"]*\)".*/\1/p' | head -n1; }
wait_state() {
  # Wait up to 30s for a settled state, then print it.
  local s="" i
  for i in $(seq 30); do
    s=$(backend)
    case "$s" in Running|Stopped|NeedsLogin|NeedsMachineAuth) break;; esac
    [ "$i" -lt 30 ] && sleep 1
  done
  printf '%s' "$s"
}
redact() { sed -E 's/tskey-[A-Za-z0-9_-]+/<redacted>/g'; }

# 1. Install tailscale + tailscaled if the binaries are gone, or if TS_VERSION names another version.
installed=""
if [ -x "$LIB/tailscaled" ] && [ -x "$LIB/tailscale" ]; then
  installed=$("$LIB/tailscale" version 2>/dev/null | head -n1 || true)
fi
restart_daemon=0
if [ -z "$installed" ] || { [ -n "$TS_VERSION" ] && [ "$installed" != "$TS_VERSION" ]; }; then
  case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo "unsupported arch"; exit 1;; esac
  v="$TS_VERSION"
  [ -n "$v" ] || v=$(curl -fsSL 'https://pkgs.tailscale.com/stable/?mode=json' | sed -n 's/.*"TarballsVersion": *"\([^"]*\)".*/\1/p' || true)
  [ -n "$v" ] || { echo "could not find the latest Tailscale version"; exit 1; }
  t="tailscale_${v}_${arch}.tgz"; tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  if curl -fsSL -o "$tmp/$t" "https://pkgs.tailscale.com/stable/$t" &&
     curl -fsSL -o "$tmp/$t.sha256" "https://pkgs.tailscale.com/stable/$t.sha256" &&
     [ "$(sha256sum "$tmp/$t" | cut -d' ' -f1)" = "$(tr -d '[:space:]' < "$tmp/$t.sha256")" ] &&
     tar -xzf "$tmp/$t" -C "$tmp" &&
     install -m 755 "$tmp/tailscale_${v}_${arch}/tailscale" "$LIB/tailscale.new" &&
     install -m 755 "$tmp/tailscale_${v}_${arch}/tailscaled" "$LIB/tailscaled.new" &&
     mv -f "$LIB/tailscale.new" "$LIB/tailscale" && mv -f "$LIB/tailscaled.new" "$LIB/tailscaled"; then
    echo "installed Tailscale $v into $LIB${installed:+ (was $installed)}"
    [ -n "$installed" ] && restart_daemon=1
  else
    echo "Tailscale install failed"; exit 1
  fi
  rm -rf "$tmp"; trap - EXIT
fi
# The wrapper bakes in our socket, so plain `tailscale ...` (and tincan's relay
# rediscovery, which runs `tailscale status --json`) talks to this daemon.
printf '#!/bin/sh\nexec "%s/tailscale" --socket="%s" "$@"\n' "$LIB" "$SOCK" > "$BIN/tailscale.new"
chmod 755 "$BIN/tailscale.new" && mv -f "$BIN/tailscale.new" "$BIN/tailscale"

# 2. Start tailscaled (userspace networking, no TUN, no root) if nothing answers,
#    or restart it after an upgrade. A daemon that already answers is left running.
if [ "$restart_daemon" = 1 ] || ! tsc status --json >/dev/null 2>&1; then
  if pkill -u "$(id -u)" -f "tailscaled .*--socket=$SOCK" 2>/dev/null; then sleep 2; fi
  rm -f "$SOCK"
  ( exec 9>&-; cd "$HOME" && env -u TS_AUTHKEY -u RELAY_TS_AUTHKEY nohup "$LIB/tailscaled" \
      --tun=userspace-networking --statedir="$STATEDIR" --socket="$SOCK" \
      --socks5-server="$PROXY_ADDR" --outbound-http-proxy-listen="$PROXY_ADDR" \
      >>"$LOG" 2>&1 </dev/null & )
  echo "started tailscaled"
fi
state=$(wait_state)

# 3. Bring the node up. A node that still has its identity (Stopped) comes up without a key.
#    A logged-out node logs in only with the auth key, never interactively. The key goes to
#    tailscale on stdin, so it is never written to disk or shown in the process list.
rc=0
up_args=(--hostname="$TS_HOSTNAME" --advertise-tags="$TS_TAGS" --timeout=60s)
case "$state" in
  Running) ;;
  Stopped|NeedsLogin)
    if [ "$state" = Stopped ] && [ -s "$STATEDIR/tailscaled.state" ]; then
      if ! out=$(tsc up "${up_args[@]}" 2>&1 </dev/null); then
        echo "tailscale up failed: $(printf '%s' "$out" | redact)"; exit 1
      fi
    else
      k=$(printf '%s' "${TS_AUTHKEY:-}" | tr -d '[:space:]')
      case "$k" in
        "") echo "logged out and TS_AUTHKEY is not set: ask the owner for a fresh one-off auth key"; exit 3;;
        tskey-auth-*) ;;
        *) echo "TS_AUTHKEY is not an auth key (must start with tskey-auth-)"; exit 3;;
      esac
      if ! out=$(printf '%s' "$k" | tsc up --auth-key=file:/dev/stdin "${up_args[@]}" 2>&1); then
        unset k
        if [ "$(backend)" = NeedsMachineAuth ]; then
          echo "logged in, but the device needs approval: the key is not Pre-approved"; exit 2
        fi
        echo "tailscale up failed (key expired, already used, revoked or wrong tag?): $(printf '%s' "$out" | redact)"
        echo "ask the owner for a fresh one-off, pre-approved auth key"; exit 3
      fi
      unset k
      echo "logged in with TS_AUTHKEY: remove it from the secrets now, the identity is saved in $STATEDIR"
    fi
    state=$(wait_state)
    case "$state" in
      Running) ;;
      NeedsMachineAuth) echo "logged in, but the device needs approval: the key is not Pre-approved"; rc=2;;
      *) echo "tailscale is not running after up (${state:-no answer}); see $LOG"; exit 1;;
    esac;;
  NeedsMachineAuth) echo "device needs approval in the Tailscale admin"; rc=2;;
  *) echo "tailscaled not ready (${state:-no answer}); see $LOG"; exit 1;;
esac

# 4. Tincan must be installed for the box to be useful.
if [ ! -x "$TINCAN" ]; then
  echo "tincan not found or not executable at $TINCAN: install it (or set TINCAN to its path)"; exit 1
fi

# 5. Optional: the relay, if this host runs it, as its own tailnet node (tsnet, never --listen)
#    with its identity in the home folder. TS_AUTHKEY is stripped so a lost relay state never
#    silently registers the relay as a new node with this host's tag. RELAY_TS_AUTHKEY is handed
#    to the relay as its TS_AUTHKEY only while it has no saved identity, for its first login.
#    The relay this script starts is tracked by its PID, so a TINCAN with another file name is
#    still recognized; a relay started by hand as `tincan relay` is recognized by name.
RELAY_STATE="$HOME/.config/tincan-relay"
RELAY_PID="$HOME/.cache/tincan-relay.pid"
relay_running() {
  local pid args
  pid=$(cat "$RELAY_PID" 2>/dev/null || true)
  case "$pid" in ''|*[!0-9]*) ;; *)
    if kill -0 "$pid" 2>/dev/null; then
      args=$(ps -p "$pid" -o args= 2>/dev/null || true)
      case "$args" in *" relay --state-dir $RELAY_STATE"*) return 0;; esac
    fi;;
  esac
  pgrep -u "$(id -u)" -f '(^|/)tincan relay( |$)' >/dev/null
}
if [ "$START_RELAY" = 1 ] && ! relay_running; then
  relay_args=(relay --state-dir "$RELAY_STATE")
  [ -n "$RELAY_ADMIN" ] && relay_args+=(--admin "$RELAY_ADMIN")
  relay_key=$(printf '%s' "${RELAY_TS_AUTHKEY:-}" | tr -d '[:space:]')
  [ -s "$RELAY_STATE/tsnet/tailscaled.state" ] && relay_key=""
  rm -f "$RELAY_PID"
  # The key goes to the relay in its environment, never on a command line.
  ( exec 9>&-; cd "$HOME" || exit 1
    unset TS_AUTHKEY RELAY_TS_AUTHKEY
    if [ -n "$relay_key" ]; then export TS_AUTHKEY="$relay_key"; fi
    nohup "$TINCAN" "${relay_args[@]}" \
      >>"$HOME/.cache/tincan-relay.log" 2>&1 </dev/null &
    echo "$!" > "$RELAY_PID" )
  started=0
  for _ in $(seq 10); do sleep 1; if relay_running; then started=1; break; fi; done
  # Give it a moment more and check it did not exit right after starting.
  if [ "$started" = 1 ]; then sleep 3; relay_running || started=0; fi
  if [ "$started" = 1 ]; then
    echo "started tincan relay"
    if [ -n "$relay_key" ]; then
      echo "started the relay with RELAY_TS_AUTHKEY: once it is on the tailnet, remove the key from the secrets; its identity is saved in $RELAY_STATE/tsnet"
    fi
  else
    echo "tincan relay did not start; see $HOME/.cache/tincan-relay.log"; [ "$rc" -eq 0 ] && rc=1
  fi
  unset relay_key
fi

# 6. Tincan client check.
if [ "$state" = Running ]; then
  echo "tailscale ok: $(tsc ip -4 2>/dev/null || true)"
  if ! "$TINCAN" doctor >/dev/null 2>&1; then
    echo "tincan doctor reported a problem: run tincan doctor"; [ "$rc" -eq 0 ] && rc=1
  fi
fi
exit "$rc"
