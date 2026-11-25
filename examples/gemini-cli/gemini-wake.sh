#!/bin/sh
# Wake for the gemini-cli teammate: Google's Antigravity CLI (agy) or Gemini
# CLI (gemini), run headless by "tincan listen --exec".
#
# Neither CLI has a daemon, so an idle gemini-cli needs something to start a
# fresh run when teammates' requests are waiting. With the agy engine,
# "tincan listen install --wake gemini-cli" installs this script, the wake
# library and a listener service. To run it by hand instead, copy this script
# and examples/lib/tincan-wake-lib.sh from the agent-tincan repo into one
# folder you keep, chmod +x this one, and run:
#
#   TINCAN_CONFIG=~/.config/tincan/gemini-cli.json tincan listen --exec ~/bin/gemini-wake.sh
#
# Engines (TINCAN_GEMINI_ENGINE):
#
#   agy     the default. Antigravity CLI, logged in once interactively with a
#           Google consumer account; no API key. By default it runs
#           confined: in agy's terminal sandbox (--sandbox), with per-run
#           settings (--settings, never saved) in strict permission mode
#           that allow only MCP tools and file reads, and deny every
#           command, every file write, and reads of the tincan config
#           folder and ~/.ssh. Such a teammate answers from what it knows
#           and can read, but cannot change files. To run it as a full
#           coding agent with tool approval off and nothing limiting its
#           writes, set TINCAN_GEMINI_ALLOW_UNCONFINED=1 on the command that
#           starts the listener. See docs/adapters/gemini-cli.md.
#   gemini  Gemini CLI with a paid API key in GEMINI_API_KEY (Gemini CLI has
#           not accepted consumer Google logins since 2026-06-18). It runs in
#           its own sandbox (--sandbox: seatbelt on macOS), writing only in
#           the workdir and the operator's write roots.
#
# Each run is recorded, with the workdir and the session id the engine's
# JSON output reports, in a private list beside TINCAN_CONFIG
# (<name>.wake-sessions), so the history agent leaves wake runs out.
#
# Settings, read from the listener's environment (the library's settings,
# TINCAN_WAKE_*, are listed in tincan-wake-lib.sh):
#
#   TINCAN_CONFIG                   this teammate's tincan config (default
#                                   ~/.config/tincan/gemini-cli.json)
#   TINCAN_GEMINI_ENGINE            agy (default) or gemini
#   TINCAN_GEMINI_WORKDIR           where the engine runs (~/tincan-gemini)
#   TINCAN_GEMINI_WRITE_ROOTS       extra write roots for the gemini engine,
#                                   colon-separated absolute paths
#   TINCAN_GEMINI_ALLOWED_ROOTS     where write roots may be (default
#                                   ~/code:~/tincan-gemini)
#   TINCAN_GEMINI_ALLOW_UNCONFINED  1 runs agy unconfined: tool approval off,
#                                   no per-run permission limits
#   TINCAN_GEMINI_VERSION_PATTERN   what "<engine> --version" must print
#   TINCAN_AGY_MCP_CONFIG           agy's MCP config (default
#                                   ~/.gemini/config/mcp_config.json)
#   TINCAN_AGY_PRINT_TIMEOUT        passed to agy --print-timeout when set
#                                   (agy sets no limit by default; the wake's
#                                   own TINCAN_WAKE_TIMEOUT still applies)
#   TINCAN_GEMINI_SETTINGS          Gemini CLI settings (default
#                                   ~/.gemini/settings.json)
#   TINCAN_GEMINI_JSON_TOOL         jq or python3, to read the MCP configs
#                                   (default jq when on PATH)
#   AGY_BIN, GEMINI_BIN             the engine binaries (agy, gemini)
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/gemini-cli.json}"
export TINCAN_CONFIG

# The library sits next to this script, or in the repo's examples/lib.
here=$(dirname -- "$0")
if [ -z "${TINCAN_WAKE_LIB:-}" ]; then
  TINCAN_WAKE_LIB=$here/tincan-wake-lib.sh
  [ -f "$TINCAN_WAKE_LIB" ] || TINCAN_WAKE_LIB=$here/../lib/tincan-wake-lib.sh
fi
if [ ! -f "$TINCAN_WAKE_LIB" ]; then
  echo "gemini-cli-wake: tincan-wake-lib.sh not found; copy examples/lib/tincan-wake-lib.sh next to this script" >&2
  exit 2
fi
# shellcheck source=SCRIPTDIR/../lib/tincan-wake-lib.sh
. "$TINCAN_WAKE_LIB"

# TINCAN_CONFIG itself is passed on as given; the wake's own uses of it
# need an absolute path, since the run is started from the workdir.
cfg=$(tincan_wake_config_path "$TINCAN_CONFIG")
SESSIONS=$(tincan_wake_beside_config "$cfg" .wake-sessions)

ENGINE=${TINCAN_GEMINI_ENGINE:-agy}
WORKDIR=${TINCAN_GEMINI_WORKDIR:-$HOME/tincan-gemini}
ALLOWED_ROOTS=${TINCAN_GEMINI_ALLOWED_ROOTS:-$HOME/code:$HOME/tincan-gemini}
WRITE_ROOTS=${TINCAN_GEMINI_WRITE_ROOTS:-}
AGY_MCP_CONFIG=${TINCAN_AGY_MCP_CONFIG:-$HOME/.gemini/config/mcp_config.json}
GEMINI_SETTINGS=${TINCAN_GEMINI_SETTINGS:-$HOME/.gemini/settings.json}

tincan_wake_init gemini-cli
tincan_wake_begin

case $ENGINE in
  agy)
    BIN=${AGY_BIN:-agy}
    VERSION_PATTERN=${TINCAN_GEMINI_VERSION_PATTERN:-[Aa]ntigravity|agy|[0-9]+\.[0-9]+}
    # The global config agy mcp add writes, and the workspace one agy reads
    # from the directory it runs in.
    MCP_FILES="$AGY_MCP_CONFIG
$WORKDIR/.agents/mcp_config.json"
    ;;
  gemini)
    BIN=${GEMINI_BIN:-gemini}
    VERSION_PATTERN=${TINCAN_GEMINI_VERSION_PATTERN:-^v?[0-9]+\.[0-9]+}
    # User settings, and the project settings Gemini CLI reads from the
    # directory it runs in.
    MCP_FILES="$GEMINI_SETTINGS
$WORKDIR/.gemini/settings.json"
    ;;
  *)
    tincan_wake_refuse "TINCAN_GEMINI_ENGINE is \"$ENGINE\"; set it to agy or gemini"
    ;;
esac

if [ "$ENGINE" = gemini ] && [ -z "${GEMINI_API_KEY:-}" ]; then
  tincan_wake_refuse "the gemini engine needs GEMINI_API_KEY in the listener's environment (Gemini CLI no longer accepts Google account logins); set it, or use TINCAN_GEMINI_ENGINE=agy"
fi
AGY_UNCONFINED=
if [ "$ENGINE" = agy ] && [ "${TINCAN_GEMINI_ALLOW_UNCONFINED:-}" = 1 ]; then
  AGY_UNCONFINED=1
fi

tincan_wake_check_binary "$BIN" "$VERSION_PATTERN"

mkdir -p "$WORKDIR"

# gemini_json_tool prints the JSON reader to use, jq or python3
# (TINCAN_GEMINI_JSON_TOOL, else jq when on PATH), or nothing when that
# tool is not on PATH.
gemini_json_tool() {
  _tool=${TINCAN_GEMINI_JSON_TOOL:-}
  if [ -z "$_tool" ]; then
    if command -v jq >/dev/null 2>&1; then _tool=jq; else _tool=python3; fi
  fi
  case $_tool in
    jq | python3) command -v "$_tool" >/dev/null 2>&1 && echo "$_tool" ;;
  esac
  return 0
}

# gemini_mcp_json FILE... prints the mcpServers of each JSON config that
# exists as the library's listing lines: name, TINCAN_CONFIG, command.
# It needs jq or python3; TINCAN_GEMINI_JSON_TOOL picks one.
# shellcheck disable=SC2329 # run through gemini_mcp_list
gemini_mcp_json() {
  _tool=$(gemini_json_tool) || _tool=
  for f in "$@"; do
    [ -e "$f" ] || continue
    if [ "$_tool" = jq ] && command -v jq >/dev/null 2>&1; then
      jq -r '(.mcpServers // {}) | to_entries[]
        | [.key,
           ((.value.env // {}).TINCAN_CONFIG // "" | tostring),
           ([.value.command // .value.url // .value.httpUrl // .value.serverUrl // ""]
             + ((.value.args // []) | map(tostring)) | join(" "))]
        | map(gsub("[\t\n]"; " ")) | join("\t")' "$f" || return 1
    elif [ "$_tool" = python3 ] && command -v python3 >/dev/null 2>&1; then
      python3 -c '
import json, sys
servers = json.load(open(sys.argv[1])).get("mcpServers") or {}
for name, v in servers.items():
    env = v.get("env") or {}
    cmd = [v.get("command") or v.get("url") or v.get("httpUrl") or v.get("serverUrl") or ""]
    cmd += [str(a) for a in (v.get("args") or [])]
    row = [name, str(env.get("TINCAN_CONFIG") or ""), " ".join(cmd)]
    print("\t".join(x.replace("\t", " ").replace("\n", " ") for x in row))
' "$f" || return 1
    else
      tincan_wake_log "reading $f needs jq or python3 on PATH"
      return 1
    fi
  done
}

# shellcheck disable=SC2329 # run by tincan_wake_check_identity
gemini_mcp_list() {
  _old_ifs=$IFS
  IFS='
'
  set -f
  # shellcheck disable=SC2086 # one file per line
  set -- $MCP_FILES
  set +f
  IFS=$_old_ifs
  gemini_mcp_json "$@"
}

if [ -z "$(gemini_json_tool)" ]; then
  tincan_wake_refuse "reading the engine's MCP config needs jq or python3 on the listener's PATH (or TINCAN_GEMINI_JSON_TOOL names one that is missing)"
fi
tincan_wake_check_identity gemini_mcp_list

# gemini_attachment_dir prints where "tincan mcp" saves the attachments this
# teammate receives: attachments/<agent> beside TINCAN_CONFIG, with the
# agent name read from the config ("default" when it is missing or not a
# plain name, as tincan does).
gemini_attachment_dir() {
  _cfg=$cfg
  _agent=
  if [ -r "$_cfg" ]; then
    case $(gemini_json_tool) in
      jq) _agent=$(jq -r '.agent // "" | tostring' "$_cfg" 2>/dev/null) || _agent= ;;
      python3) _agent=$(python3 -c '
import json, sys
print(str(json.load(open(sys.argv[1])).get("agent") or ""))
' "$_cfg" 2>/dev/null) || _agent= ;;
    esac
  fi
  if ! printf '%s\n' "$_agent" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$' || [ "$_agent" = . ] || [ "$_agent" = .. ]; then
    _agent=default
  fi
  printf '%s\n' "$(dirname -- "$_cfg")/attachments/$_agent"
}

# gemini_agy_deny_reads prints, one per line, the paths a confined agy run
# may not read: everything in the tincan config folder, which holds every
# co-located teammate's key, except this teammate's own attachments folder,
# and ~/.ssh. agy resolves symlinks and ./ before matching a deny rule, but
# it matches case exactly, so on a case-insensitive volume these denies are
# best effort.
gemini_agy_deny_reads() {
  _cfgdir=$(CDPATH='' cd -P -- "$(dirname -- "$cfg")" && pwd -P)
  _own=$(basename -- "$(gemini_attachment_dir)")
  for _e in "$_cfgdir"/* "$_cfgdir"/.[!.]* "$_cfgdir"/..?*; do
    [ -e "$_e" ] || [ -L "$_e" ] || continue
    if [ "$_e" = "$_cfgdir/attachments" ] && [ -d "$_e" ] && [ ! -L "$_e" ]; then
      for _a in "$_e"/* "$_e"/.[!.]* "$_e"/..?*; do
        [ -e "$_a" ] || [ -L "$_a" ] || continue
        [ "$_a" = "$_e/$_own" ] || printf '%s\n' "$_a"
      done
    else
      printf '%s\n' "$_e"
    fi
  done
  printf '%s\n' "$(CDPATH='' cd -P -- "$HOME" && pwd -P)/.ssh"
}

# gemini_agy_settings prints the per-run agy settings for a confined run:
# strict permission mode, where agy denies any tool call no rule allows
# (headless runs cannot ask). Only whole-category allow rules take effect in
# strict mode (agy 1.5.16), so it allows MCP tools (the identity check above
# vouches for every configured server, and the plugin check below for the
# rest) and file reads, which agy itself needs before calling an MCP tool.
# Deny rules win over allow rules, including any merged in from the owner's
# own settings.
gemini_agy_settings() {
  _deny=$(gemini_agy_deny_reads) || return 1
  _old_ifs=$IFS
  IFS='
'
  set -f
  # shellcheck disable=SC2086 # one path per line
  set -- $_deny
  set +f
  IFS=$_old_ifs
  case $(gemini_json_tool) in
    jq) jq -cn '{toolPermission: "strict", permissions: {allow: ["mcp(*)", "read_file(/)"],
        deny: (["command(*)", "write_file(/)"] + ($ARGS.positional | map("read_file(\(.))")))}}' --args "$@" ;;
    python3) python3 -c '
import json, sys
print(json.dumps({"toolPermission": "strict", "permissions": {
    "allow": ["mcp(*)", "read_file(/)"],
    "deny": ["command(*)", "write_file(/)"] + ["read_file(%s)" % p for p in sys.argv[1:]]}}))
' "$@" ;;
    *) return 1 ;;
  esac
}

# gemini_denied_actions FILE prints the names of the actions an agy run's
# JSON output lists under denied_actions, space separated.
gemini_denied_actions() {
  case $(gemini_json_tool) in
    jq) jq -r '[(.denied_actions // [])[] | .action // empty | tostring] | join(" ")' "$1" 2>/dev/null || : ;;
    python3) python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
print(" ".join(str(a.get("action")) for a in (d.get("denied_actions") or []) if isinstance(a, dict) and a.get("action")))
' "$1" 2>/dev/null || : ;;
  esac
  return 0
}

if [ "$ENGINE" = agy ] && [ -z "$AGY_UNCONFINED" ]; then
  LIMITS="You can read files, except the tincan config folder (your own received attachments in it are readable) and ~/.ssh, but you cannot write files or run commands in this run: if a request needs either, reply with status failed and say that this teammate runs read-only, rather than trying."
else
  LIMITS="You can read anywhere, but write only in your working directory and in write roots your operator has opened; if work needs to be written somewhere else, say so in your reply rather than writing there."
fi

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan item(s) waiting: requests from teammates, or replies to requests you sent. Call check_inbox. It shows replies to your requests first, with what you asked: finish the work that was waiting on each one. Then, for each request, handle it the way you would handle a request from your owner, and call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask; it may return before the answer does, and you do not have to wait for it: you will be woken again when the reply arrives. $LIMITS"

if [ "$ENGINE" = agy ]; then
  if [ -n "$WRITE_ROOTS" ]; then
    tincan_wake_log "TINCAN_GEMINI_WRITE_ROOTS is ignored for agy, which has no write confinement to open roots in"
  fi
  if [ -n "$AGY_UNCONFINED" ]; then
    tincan_wake_log "running agy unconfined (TINCAN_GEMINI_ALLOW_UNCONFINED=1): tool approval is off and nothing limits its writes"
    set -- "$BIN" --output-format json --dangerously-skip-permissions
  else
    # A plugin's MCP servers load in every agy run but are not in
    # mcp_config.json, so the identity check cannot vet them, and mcp(*)
    # would allow their tools, including ones that write or run commands.
    plugins=$("$BIN" plugin list --json 2>/dev/null) || tincan_wake_refuse "could not list agy's plugins (agy plugin list --json failed)"
    if [ "$(printf '%s' "$plugins" | tr -d ' \t\r\n')" != '[]' ]; then
      tincan_wake_refuse "agy has plugins installed, and a confined wake cannot vet their MCP servers; remove them with agy plugin uninstall, or set TINCAN_GEMINI_ALLOW_UNCONFINED=1"
    fi
    settings=$(gemini_agy_settings) || tincan_wake_refuse "could not build agy's per-run settings (needs jq or python3)"
    set -- "$BIN" --sandbox --output-format json --settings "$settings"
  fi
  if [ -n "${TINCAN_AGY_PRINT_TIMEOUT:-}" ]; then
    set -- "$@" --print-timeout "$TINCAN_AGY_PRINT_TIMEOUT"
  fi
else
  # Only the vetted servers may start, whatever else a settings file or an
  # extension adds: the tincan server the identity check found, plus the
  # operator's allowed servers.
  set -- "$BIN" --sandbox --approval-mode=yolo --output-format json --allowed-mcp-server-names "$tincan_wake_server"
  for s in $(printf '%s' "${TINCAN_WAKE_ALLOWED_SERVERS:-}" | tr ',' ' '); do
    set -- "$@" --allowed-mcp-server-names "$s"
  done
  roots=$(tincan_wake_write_roots "$ALLOWED_ROOTS" "$WRITE_ROOTS")
  while IFS= read -r d; do
    [ -n "$d" ] && set -- "$@" --include-directories "$d"
  done <<EOF
$roots
EOF
  # tincan mcp runs inside the sandbox too, and saves received attachments
  # beside TINCAN_CONFIG, outside the workdir: open that one directory.
  att=$(gemini_attachment_dir)
  if { mkdir -p "$att" && chmod 700 "$att"; } 2>/dev/null && attc=$(tincan_wake_canonical_dir "$att"); then
    set -- "$@" --include-directories "$attc"
  else
    tincan_wake_log "cannot create the attachments directory $att; received attachments will not be saved"
  fi
fi

# The engine's output is kept in the wake's private state directory, so an
# authentication failure (exit 41 from Gemini CLI, or a login message on
# the engine's stderr) can be told apart from other failures, then passed
# on to the listener's log.
state=$(CDPATH='' cd -P -- "$TINCAN_WAKE_STATE_DIR" && pwd -P)
out=$state/out.$$
err=$state/err.$$
# A wake stopped mid-run (the listener stopping) exits through the
# library's EXIT trap: remove the captured output there too, then run the
# library's own cleanup (the engine's process tree and the lock).
trap 'rm -f "$out" "$err"; _tw_cleanup' EXIT
# The workdir is recorded before the run, so even a run the timeout kills
# is known to the history agent; the session id once the engine reports it.
cd "$WORKDIR"
workdir_c=$(pwd -P)
tincan_wake_record_session "$SESSIONS" "$workdir_c"
status=0
tincan_wake_run "$@" -p "$PROMPT" >"$out" 2>"$err" || status=$?
cat "$out"
cat "$err" >&2
if [ "$ENGINE" = agy ]; then
  denied=$(gemini_denied_actions "$out")
  [ -z "$denied" ] || tincan_wake_log "agy denied these tool calls under its permission limits: $denied"
fi
sid=$(sed -nE 's/.*"(session|conversation)_?[iI]d": *"([A-Za-z0-9._-]*)".*/\2/p' "$out" | head -n 1)
[ -z "$sid" ] || tincan_wake_record_session "$SESSIONS" "$workdir_c" "$sid"
auth=
if [ "$status" -ne 0 ] && [ "$status" -ne 124 ]; then
  if [ "$ENGINE" = gemini ] && [ "$status" -eq 41 ]; then
    auth=1
  elif grep -Eiq 'authentication required|not authenticated|unauthenticated|not logged in|log ?in (is )?required|login (has )?expired|please (re-?)?log ?in|api key not valid|invalid api key|API_KEY_INVALID' "$err"; then
    # Only the engine's own stderr: stdout carries the model's words, which
    # can say anything.
    auth=1
  fi
fi
rm -f "$out" "$err"
if [ -n "$auth" ]; then
  if [ "$ENGINE" = agy ]; then
    tincan_wake_backoff "agy is not logged in or its login expired; run agy once on this machine and log in with the owner's Google account"
  else
    tincan_wake_backoff "Gemini CLI rejected its credentials; check GEMINI_API_KEY in the listener's environment"
  fi
fi
exit "$status"
