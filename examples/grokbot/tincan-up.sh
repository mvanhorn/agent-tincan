#!/bin/bash
# examples/grokbot/tincan-up.sh: bring Tailscale (and optionally the Tincan relay) back after the host
# was wiped or moved. Idempotent; safe to run every hour. Runs as the normal user, no sudo.
#
# See docs/adapters/grokbot.md.
# Exit codes: 0 healthy | 1 unhealthy | 2 device needs approval in the Tailscale admin
#             3 auth key missing, wrong type, expired or rejected
set -u

TS_HOSTNAME="${TS_HOSTNAME:-grokbot}"        # the machine name the relay knows
TS_TAGS="${TS_TAGS:-tag:grokbot}"            # must match the tag on the auth key
TS_VERSION="${TS_VERSION:-}"                 # pin a version, or empty for latest stable
PROXY_ADDR="${PROXY_ADDR:-localhost:1055}"   # SOCKS5 + HTTP proxy into the tailnet
START_RELAY="${START_RELAY:-0}"              # 1 if this host also runs `tincan relay`

LIB="$HOME/.local/lib/tailscale"             # real binaries
BIN="$HOME/.local/bin"                       # wrapper `tailscale` (on PATH)
STATEDIR="$HOME/.config/tailscale"           # node identity: must be in the home folder
SOCK="$HOME/.cache/tailscale/tailscaled.sock"
LOG="$HOME/.cache/tailscale/tailscaled.log"
TINCAN="$BIN/tincan"

exec 9>"/tmp/tincan-up.$(id -u).lock"
if command -v flock >/dev/null && ! flock -n 9; then echo "already running"; exit 0; fi

mkdir -p "$LIB" "$BIN" "$STATEDIR" "$(dirname "$SOCK")"
chmod 700 "$STATEDIR" "$(dirname "$SOCK")"

# 1. Install tailscale + tailscaled if the binaries are gone.
if [ ! -x "$LIB/tailscaled" ] || [ ! -x "$LIB/tailscale" ]; then
  case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo "unsupported arch"; exit 1;; esac
  v="$TS_VERSION"
  [ -n "$v" ] || v=$(curl -fsSL 'https://pkgs.tailscale.com/stable/?mode=json' | sed -n 's/.*"TarballsVersion": *"\([^"]*\)".*/\1/p')
  [ -n "$v" ] || { echo "could not find the latest Tailscale version"; exit 1; }
  t="tailscale_${v}_${arch}.tgz"; tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/$t" "https://pkgs.tailscale.com/stable/$t" &&
  curl -fsSL -o "$tmp/$t.sha256" "https://pkgs.tailscale.com/stable/$t.sha256" &&
  [ "$(sha256sum "$tmp/$t" | cut -d' ' -f1)" = "$(tr -d '[:space:]' < "$tmp/$t.sha256")" ] &&
  tar -xzf "$tmp/$t" -C "$tmp" &&
  install -m 755 "$tmp/tailscale_${v}_${arch}/tailscale" "$tmp/tailscale_${v}_${arch}/tailscaled" "$LIB/" ||
    { echo "Tailscale install failed"; rm -rf "$tmp"; exit 1; }
  rm -rf "$tmp"; echo "installed Tailscale $v into $LIB"
fi
# The wrapper bakes in our socket, so plain `tailscale ...` (and tincan's relay
# rediscovery, which runs `tailscale status --json`) talks to this daemon.
printf '#!/bin/sh\nexec "%s/tailscale" --socket="%s" "$@"\n' "$LIB" "$SOCK" > "$BIN/tailscale.new" &&
  chmod 755 "$BIN/tailscale.new" && mv "$BIN/tailscale.new" "$BIN/tailscale"

tsc() { "$LIB/tailscale" --socket="$SOCK" "$@"; }
backend() { tsc status --json 2>/dev/null | sed -n 's/.*"BackendState": *"\([^"]*\)".*/\1/p' | head -n1; }

# 2. Start tailscaled (userspace networking, no TUN, no root) if nothing answers.
if ! tsc status --json >/dev/null 2>&1; then
  pkill -u "$(id -u)" -f "tailscaled .*--socket=$SOCK" 2>/dev/null && sleep 2
  rm -f "$SOCK"
  ( exec 9>&-; cd "$HOME" && env -u TS_AUTHKEY nohup "$LIB/tailscaled" \
      --tun=userspace-networking --statedir="$STATEDIR" --socket="$SOCK" \
      --socks5-server="$PROXY_ADDR" --outbound-http-proxy-listen="$PROXY_ADDR" \
      >>"$LOG" 2>&1 </dev/null & )
  echo "started tailscaled"
fi
state=""; for _ in $(seq 30); do
  state=$(backend)
  case "$state" in Running|Stopped|NeedsLogin|NeedsMachineAuth) break;; esac; sleep 1
done

# 3. Log in only when logged out, only with the auth key, never interactively.
rc=0
case "$state" in
  Running) ;;
  NeedsLogin|Stopped)
    k=$(printf '%s' "${TS_AUTHKEY:-}" | tr -d '[:space:]')
    case "$k" in
      "") echo "logged out and TS_AUTHKEY is not set"; exit 3;;
      tskey-auth-*) ;;
      *) echo "TS_AUTHKEY is not an auth key (must start with tskey-auth-)"; exit 3;;
    esac
    kf=$(umask 077; mktemp) || exit 1
    printf '%s' "$k" > "$kf"; unset k
    out=$(tsc up --auth-key="file:$kf" --hostname="$TS_HOSTNAME" \
          --advertise-tags="$TS_TAGS" --timeout=60s 2>&1); urc=$?
    rm -f "$kf"
    state=$(backend)
    if [ "$state" = NeedsMachineAuth ]; then
      echo "logged in, but the device needs approval: the key is not Pre-approved"; rc=2
    elif [ $urc -ne 0 ]; then
      echo "tailscale up failed (key expired, revoked or wrong tag?): $(printf '%s' "$out" | sed -E 's/tskey-[A-Za-z0-9_-]+/<redacted>/g')"; exit 3
    fi;;
  NeedsMachineAuth) echo "device needs approval in the Tailscale admin"; rc=2;;
  *) echo "tailscaled not ready (${state:-no answer}); see $LOG"; exit 1;;
esac

# 4. Optional: the relay, if this host runs it. TS_AUTHKEY is stripped so a lost relay
#    state never silently registers the relay as a new node with this host's tag.
if [ "$START_RELAY" = 1 ] && ! pgrep -u "$(id -u)" -f "^$TINCAN relay" >/dev/null; then
  ( exec 9>&-; cd "$HOME" && env -u TS_AUTHKEY nohup "$TINCAN" relay \
      --state-dir "$HOME/.config/tincan-relay" >>"$HOME/.cache/tincan-relay.log" 2>&1 </dev/null & )
  sleep 5; echo "started tincan relay"
fi

# 5. Tincan client check.
if [ "$state" = Running ]; then
  echo "tailscale ok: $(tsc ip -4 2>/dev/null)"
  if [ -x "$TINCAN" ] && ! "$TINCAN" doctor >/dev/null 2>&1; then
    echo "tincan doctor reported a problem: run tincan doctor"; [ $rc -eq 0 ] && rc=1
  fi
fi
exit $rc
