#!/bin/sh
# Wake for OpenAI Codex CLI.
#
# Codex CLI has no daemon: "codex exec" runs one prompt and exits, so an idle
# Codex needs something else to start a fresh run when teammates' requests
# are waiting. Use this with:
#
#   tincan listen --exec ~/agent-tincan/examples/codex/codex-wake.sh
#
# "tincan listen" runs this script whenever requests, or unread replies to
# codex's own requests, are waiting and passes the count in TINCAN_WAITING. It does not take the requests itself, and it
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

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan item(s) waiting: requests from teammates, or replies to requests you sent. Call check_inbox. It shows replies to your requests first, with what you asked: finish the work that was waiting on each one. Then, for each request, handle it the way you would handle a request from your owner, and call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask; it may return before the answer does, and you do not have to wait for it: you will be woken again when the reply arrives. Before you touch any checkout for work that continues a prior thread (for example \"the work you did yesterday\"), run \"tincan history codex --list 20 --all\" to find that thread, when it was last updated, and its real working directory (cwd), and work in that cwd instead of guessing a path. You can read anywhere, but you can write only in your working directory and in write roots your operator has opened; if that cwd is not writable, say so in your reply rather than redoing the work somewhere else."

# Write roots. Reading is allowed everywhere under workspace-write; writing
# is confined to CODEX_WORKDIR plus whatever --add-dir opens. The operator
# (not the model) lists extra write roots in TINCAN_CODEX_WRITE_ROOTS,
# colon-separated absolute paths. Each is resolved to its canonical path
# (symlinks and ../ followed, via cd -P in a subshell, since macOS has no
# realpath -m) and becomes --add-dir only if that canonical path is at or
# under one of TINCAN_CODEX_ALLOWED_ROOTS (colon-separated, default
# $HOME/Documents/Codex:$HOME/code:$HOME/tincan-codex), also canonicalized.
# Anything else is skipped with a note on stderr.
ALLOWED_ROOTS="${TINCAN_CODEX_ALLOWED_ROOTS:-$HOME/Documents/Codex:$HOME/code:$HOME/tincan-codex}"
WRITE_ROOTS="${TINCAN_CODEX_WRITE_ROOTS:-}"

# canonical_dir prints the physical path of an existing directory, or fails.
canonical_dir() {
  case "$1" in
    /*) ;;
    *) return 1 ;;
  esac
  (CDPATH='' cd -P -- "$1" 2>/dev/null && pwd -P)
}

# under_allowed_root succeeds if canonical path $1 is at or under an allowed root.
under_allowed_root() {
  for allowed in $ALLOWED_ROOTS; do
    [ -n "$allowed" ] || continue
    allowed_canon=$(canonical_dir "$allowed") || continue
    [ "$allowed_canon" = / ] && allowed_canon=
    case "$1/" in
      "$allowed_canon"/*) return 0 ;;
    esac
  done
  return 1
}

# workspace-write plus approval_policy=never lets this unattended run act
# without a human present to answer approval prompts; commands still run
# inside the sandbox's workspace-write policy, confined to CODEX_WORKDIR (and
# the checked write roots above), not full disk access. This is a real
# trade-off (model-generated commands execute without asking first), so it
# deliberately stops short of --dangerously-bypass-approvals-and-sandbox,
# which would drop the sandbox too. skip-git-repo-check lets this run from a
# plain working directory instead of a git checkout. `codex exec` has no
# --ask-for-approval flag (that is an interactive-mode-only option); the
# non-interactive equivalent is the approval_policy config key via -c.
#
# sandbox_workspace_write.network_access=true lets commands in the sandbox
# reach the network, so gh and git can talk to GitHub. This is a deliberate
# trade-off: a model-run command can now send data off the machine, not only
# write inside the write roots. See docs/adapters/codex.md.
set -- "$CODEX_BIN" exec \
  --sandbox workspace-write \
  -c approval_policy=never \
  -c sandbox_workspace_write.network_access=true \
  --skip-git-repo-check \
  --cd "$CODEX_WORKDIR"

old_ifs=$IFS
IFS=:
set -f
for root in $WRITE_ROOTS; do
  [ -n "$root" ] || continue
  if ! canon=$(canonical_dir "$root"); then
    echo "codex-wake: skipping write root $root: not an absolute path to an existing directory" >&2
    continue
  fi
  if under_allowed_root "$canon"; then
    set -- "$@" --add-dir "$canon"
  else
    echo "codex-wake: refusing write root $root (resolves to $canon): not under an allowed root ($ALLOWED_ROOTS)" >&2
  fi
done
set +f
IFS=$old_ifs

# Run as a child, not exec: the EXIT trap above has to fire after codex exits
# so it can release the lock directory for the next wake.
"$@" "$PROMPT" </dev/null
status=$?
exit "$status"
