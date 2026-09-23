#!/bin/sh
# Wake for OpenAI Codex CLI.
#
# Codex CLI has no daemon: "codex exec" runs one prompt and exits, so an idle
# Codex needs something else to start a fresh run when teammates' requests
# are waiting. Use this with:
#
#   tincan listen --exec ~/agent-tincan/examples/codex/codex-wake.sh
#
# "tincan listen" runs this script whenever requests are waiting and passes
# the count in TINCAN_WAITING. It does not take the requests itself, and it
# does not wait for a previous run of this script to finish before nudging
# again, so this script takes a lock and exits quietly if a run is already
# in progress; requests stay queued for the next nudge.
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/codex.json}"
export TINCAN_CONFIG

CODEX_BIN="${CODEX_BIN:-codex}"
LOCK_DIR="${TINCAN_CODEX_LOCK_DIR:-${TMPDIR:-/tmp}/tincan-codex-wake.lock}"
CODEX_WORKDIR="${TINCAN_CODEX_WORKDIR:-$HOME/tincan-codex}"

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "codex-wake: a run is already in progress, leaving requests queued" >&2
  exit 0
fi
trap 'rmdir "$LOCK_DIR" 2>/dev/null || true' EXIT INT TERM

mkdir -p "$CODEX_WORKDIR"

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan request(s) waiting from teammates. Call check_inbox, and for each request handle it the way you would handle a request from your owner, then call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask and wait for its reply inline, or poll get_reply, before you move on: you will not be woken again just because that reply arrived."

# workspace-write plus approval_policy=never lets this unattended run act
# without a human present to answer approval prompts; commands still run
# inside the sandbox's workspace-write policy, confined to CODEX_WORKDIR (and
# anywhere else --add-dir opens up), not full disk access. This is a real
# trade-off (model-generated commands execute without asking first), so it
# deliberately stops short of --dangerously-bypass-approvals-and-sandbox,
# which would drop the sandbox too. skip-git-repo-check lets this run from a
# plain working directory instead of a git checkout. `codex exec` has no
# --ask-for-approval flag (that is an interactive-mode-only option); the
# non-interactive equivalent is the approval_policy config key via -c.
#
# Run as a child, not exec: the EXIT trap above has to fire after codex exits
# so it can release the lock directory for the next wake.
"$CODEX_BIN" exec \
  --sandbox workspace-write \
  -c approval_policy=never \
  --skip-git-repo-check \
  --cd "$CODEX_WORKDIR" \
  "$PROMPT" </dev/null
status=$?
exit "$status"
