#!/bin/sh
# Entrypoint for the headless Claude Code runtime (citadel-cli#432).
#
# The container runs the wrapper + `claude` as the NON-root `claude` user
# (uid 10001), and the compose file bind-mounts the node dir
# ~/citadel-cache/claudecode onto CLAUDE_CONFIG_DIR (/home/claude/.claude) so
# Claude Code state persists across restarts. A bind-mount source is owned by
# whoever created it on the host (root or the operator's uid), NOT 10001 -- so
# without this fixup, Claude Code's first-run config/session writes into the
# mounted dir fail with EACCES.
#
# Fix: start as root (this script's PID via tini), chown the state dir to the
# claude user, then drop to that user via gosu to exec the server. Claude Code
# refuses --dangerously-skip-permissions as root, so dropping to a real user is
# also required for the turn to run at all.
set -e

STATE_DIR="${CLAUDE_CONFIG_DIR:-/home/claude/.claude}"
mkdir -p "$STATE_DIR"
# Only chown when needed (a large persisted state dir shouldn't be re-chowned
# top-to-bottom on every boot). If the top dir is already claude-owned, assume
# the tree is fine.
if [ "$(stat -c '%u' "$STATE_DIR")" != "10001" ]; then
    chown -R claude:claude "$STATE_DIR"
fi

exec gosu claude "$@"
