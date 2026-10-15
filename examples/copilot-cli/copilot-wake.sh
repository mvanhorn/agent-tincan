#!/bin/sh
# Wake for the copilot-cli teammate: GitHub Copilot CLI (copilot), run
# headless by "tincan listen --exec".
#
# Copilot CLI has no daemon, so an idle copilot-cli needs something to
# start a fresh run when teammates' requests are waiting. Copy this script
# and examples/lib/tincan-wake-lib.sh from the agent-tincan repo (they are
# not in the release downloads) into one folder you keep, chmod +x this one,
# and run:
#
#   TINCAN_CONFIG=~/.config/tincan/copilot-cli.json tincan listen --exec ~/bin/copilot-wake.sh
#
# Copilot runs with a home of its own: COPILOT_HOME is <wake home>/.copilot,
# so the owner's ~/.copilot/mcp-config.json, plugins, hooks and trusted
# folders are not loaded. The wake writes the agent-tincan server (this
# teammate's TINCAN_CONFIG) to a file in its state directory and hands it
# over with --additional-mcp-config; --disable-builtin-mcps drops the
# built-in GitHub servers. Any server in the wake home's own
# mcp-config.json stops the wake unless TINCAN_WAKE_ALLOWED_SERVERS lists
# it.
#
# Tool approval is an allowlist: --allow-tool 'agent-tincan(<tool>)' for
# each Agent Tincan tool and nothing else, never --allow-all-tools. In -p
# mode Copilot denies a tool call that is not allowed, so a run is meant to
# read in its workdir and use the Agent Tincan tools, nothing else.
# COPILOT_ALLOW_ALL in the listener's environment is cleared for the run.
#
# Log in once for the wake home (COPILOT_HOME=<wake home>/.copilot copilot
# login), or set COPILOT_GITHUB_TOKEN in the listener's environment. See
# docs/adapters/copilot-cli.md.
#
# Each run is a new session whose id the wake picks up front and records,
# with the workdir, in a private list beside TINCAN_CONFIG
# (<name>.wake-sessions), so the history agent leaves wake runs out.
#
# Settings, read from the listener's environment (the library's settings,
# TINCAN_WAKE_*, are listed in tincan-wake-lib.sh):
#
#   TINCAN_CONFIG                  this teammate's tincan config (default
#                                  ~/.config/tincan/copilot-cli.json)
#   TINCAN_COPILOT_WAKE_HOME       the wake home (default <name>.wake beside
#                                  TINCAN_CONFIG)
#   TINCAN_COPILOT_WORKDIR         where copilot runs (default
#                                  <wake home>/work)
#   TINCAN_COPILOT_VERSION_PATTERN what "copilot --version" must print
#                                  (default "GitHub Copilot CLI X.Y.Z")
#   TINCAN_COPILOT_JSON_TOOL       jq or python3, to read the MCP configs
#                                  (default jq when on PATH)
#   COPILOT_BIN                    the copilot binary (default copilot)
#   TINCAN_WAKE_STATE_DIR          as in the library, but by default
#                                  <name>.wake-state beside TINCAN_CONFIG
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/copilot-cli.json}"

# The library sits next to this script, or in the repo's examples/lib.
here=$(dirname -- "$0")
if [ -z "${TINCAN_WAKE_LIB:-}" ]; then
  TINCAN_WAKE_LIB=$here/tincan-wake-lib.sh
  [ -f "$TINCAN_WAKE_LIB" ] || TINCAN_WAKE_LIB=$here/../lib/tincan-wake-lib.sh
fi
if [ ! -f "$TINCAN_WAKE_LIB" ]; then
  echo "copilot-cli-wake: tincan-wake-lib.sh not found; copy examples/lib/tincan-wake-lib.sh next to this script" >&2
  exit 2
fi
# shellcheck source=SCRIPTDIR/../lib/tincan-wake-lib.sh
. "$TINCAN_WAKE_LIB"

# The run is started from the workdir, so every path the wake keeps is
# made absolute first.
TINCAN_CONFIG=$(tincan_wake_config_path "$TINCAN_CONFIG")
export TINCAN_CONFIG

WAKE_HOME=${TINCAN_COPILOT_WAKE_HOME:-$(tincan_wake_beside_config "$TINCAN_CONFIG" .wake)}
WAKE_HOME=${WAKE_HOME%/}
# A relative wake home is taken from where the listener started, so the
# COPILOT_HOME the wake checks is the one Copilot runs with after the cd.
case $WAKE_HOME in /*) ;; *) WAKE_HOME=$(pwd -P)/$WAKE_HOME ;; esac
COPILOT_HOME_DIR=$WAKE_HOME/.copilot
WORKDIR=${TINCAN_COPILOT_WORKDIR:-$WAKE_HOME/work}
SESSIONS=$(tincan_wake_beside_config "$TINCAN_CONFIG" .wake-sessions)
BIN=${COPILOT_BIN:-copilot}
VERSION_PATTERN=${TINCAN_COPILOT_VERSION_PATTERN:-^GitHub Copilot CLI [0-9]+[.][0-9]+[.][0-9]+}
: "${TINCAN_WAKE_STATE_DIR:=$(tincan_wake_beside_config "$TINCAN_CONFIG" .wake-state)}"
case $TINCAN_WAKE_STATE_DIR in /*) ;; *) TINCAN_WAKE_STATE_DIR=$(pwd -P)/$TINCAN_WAKE_STATE_DIR ;; esac

tincan_wake_init copilot-cli
tincan_wake_begin

if ! { mkdir -p "$COPILOT_HOME_DIR" "$WORKDIR" && chmod 700 "$WAKE_HOME"; } 2>/dev/null ||
  ! workdir_c=$(tincan_wake_canonical_dir "$WORKDIR"); then
  tincan_wake_refuse "cannot create the wake home $WAKE_HOME or the workdir $WORKDIR (set TINCAN_COPILOT_WAKE_HOME to an absolute path this user can write)"
fi
if [ "$(tincan_wake_canonical_dir "$WAKE_HOME")" = "$(tincan_wake_canonical_dir "$HOME" || echo "$HOME")" ]; then
  tincan_wake_refuse "the wake home $WAKE_HOME is the owner's home; set TINCAN_COPILOT_WAKE_HOME to a directory of its own"
fi

# The owner's environment may allow every tool; the wake's allowlist is
# the only approval a run gets.
unset COPILOT_ALLOW_ALL COPILOT_ASSISTED_APPROVAL

tincan_wake_check_binary env "$VERSION_PATTERN" COPILOT_HOME="$COPILOT_HOME_DIR" "$BIN" --version

JSON_TOOL=$(tincan_wake_json_tool "${TINCAN_COPILOT_JSON_TOOL:-}")
if [ -z "$JSON_TOOL" ]; then
  tincan_wake_refuse "reading Copilot's MCP config needs jq or python3 on the listener's PATH (or TINCAN_COPILOT_JSON_TOOL names one that is missing)"
fi

# The agent-tincan server, written before every run to the wake's private
# state directory.
tincan_wake_resolve_tincan
state=$(CDPATH='' cd -P -- "$TINCAN_WAKE_STATE_DIR" && pwd -P)
MCP_CONFIG=$state/mcp-config.json
tincan_wake_write_file "$MCP_CONFIG" <<EOF
{"mcpServers":{"agent-tincan":{"type":"local","command":$(tincan_wake_json_string "$tincan_wake_tincan"),"args":["mcp"],"env":{"TINCAN_CONFIG":$(tincan_wake_json_string "$TINCAN_CONFIG")},"tools":["*"]}}}
EOF

# Copilot loads the wake home's mcp-config.json and the wake's file.
# shellcheck disable=SC2329 # run by tincan_wake_check_identity
copilot_mcp_list() {
  tincan_wake_mcp_json "$JSON_TOOL" "$COPILOT_HOME_DIR/mcp-config.json" "$MCP_CONFIG"
}
tincan_wake_check_identity copilot_mcp_list

# The session id is chosen up front and recorded before the run, so even a
# run the timeout kills is known to the history agent.
SID=$(tincan_wake_uuid) || tincan_wake_refuse "cannot make a session id (needs uuidgen, /proc/sys/kernel/random/uuid or python3)"
tincan_wake_record_session "$SESSIONS" "$workdir_c" "$SID"

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan item(s) waiting: requests from teammates, or replies to requests you sent. Call check_inbox. It shows replies to your requests first, with what you asked: finish the work that was waiting on each one. Then, for each request, handle it the way you would handle a request from your owner, and call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask; it may return before the answer does, and you do not have to wait for it: you will be woken again when the reply arrives. In this run you can read files in your working directory and use the Agent Tincan tools, nothing else; if a request needs more (editing files, running commands), say so in your reply."

set -- "$BIN" -p "$PROMPT" -s --disable-builtin-mcps --additional-mcp-config "@$MCP_CONFIG" --session-id "$SID"
for tool in $tincan_wake_tools; do
  set -- "$@" --allow-tool "$tincan_wake_server($tool)"
done

out=$state/out.$$
err=$state/err.$$
trap 'rm -f "$out" "$err"; _tw_cleanup' EXIT
cd "$workdir_c" || tincan_wake_refuse "cannot enter the workdir $workdir_c"
status=0
tincan_wake_run env COPILOT_HOME="$COPILOT_HOME_DIR" "$@" >"$out" 2>"$err" || status=$?
cat "$out"
cat "$err" >&2
auth=
# Only Copilot's stderr: its stdout carries the model's words, which can
# say anything.
if [ "$status" -ne 0 ] && [ "$status" -ne 124 ] &&
  grep -Eiq 'not (logged|signed) in|no authentication|authentication (failed|required)|unauthorized|(^|[^0-9.,])401([^0-9]|$)|copilot login|/login' "$err"; then
  auth=1
fi
rm -f "$out" "$err"
if [ -n "$auth" ]; then
  tincan_wake_backoff "Copilot CLI is not logged in for this wake or its login expired; run COPILOT_HOME=$COPILOT_HOME_DIR copilot login on this machine, or set COPILOT_GITHUB_TOKEN in the listener's environment"
fi
exit "$status"
