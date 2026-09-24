#!/usr/bin/env bash
# Fallback wake for Claude Code in cmux, for when channels are unavailable.
# Opens a new cmux surface and starts a Claude Code session that picks up the
# waiting Agent Tincan requests. Same cmux control-plane calls as
# agentmail-to-claude-code (cmux socketControlMode must allow automation; see
# its setup_cmux.py).
#
# Copy it from the repo (examples/claude-code/cmux-wake.sh; it is not in the
# release downloads) to ~/bin, chmod +x it, and use:
#   tincan listen --exec ~/bin/cmux-wake.sh
set -euo pipefail

CMUX_BIN="${CMUX_BIN:-/Applications/cmux.app/Contents/Resources/bin/cmux}"
PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan request(s) from teammates. Call check_inbox, handle each one as you would a request from the owner, and reply to each."

surface=""
for attempt in 1 2 3; do
  if out=$("$CMUX_BIN" rpc surface.create 2>/dev/null); then
    surface=$(printf '%s' "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("surface_id",""))')
    [ -n "$surface" ] && break
  fi
  sleep 2
done
[ -n "$surface" ] || { echo "cmux-wake: could not open a cmux surface" >&2; exit 1; }

sleep 2
if [ "${CMUX_AUTOSTARTS_CLAUDE:-0}" = "1" ]; then
  text="$PROMPT"      # the new surface already runs Claude Code
else
  text="claude \"$PROMPT\""
fi
payload=$(python3 -c 'import json,sys; print(json.dumps({"surface_id": sys.argv[1], "text": sys.argv[2]}))' "$surface" "$text")
"$CMUX_BIN" rpc surface.send_text "$payload" >/dev/null
"$CMUX_BIN" rpc surface.send_key "$(python3 -c 'import json,sys; print(json.dumps({"surface_id": sys.argv[1], "key": "enter"}))' "$surface")" >/dev/null
