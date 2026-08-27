#!/bin/sh
# Entrypoint for the Citadel meeting media stack (aceteam-ai/citadel-cli#514).
#
# The container runs Xvfb, PulseAudio, meetingd, and per-session Chromium/ffmpeg
# as the non-root `bot` user. Two dirs are bind-mounted from the host (the
# signed-in Chrome profile at /profile and the shared node workspace at
# /workspace). The WAV recording is written into /workspace and then READ back by
# the node process (end-of-call transcribe + rolling-window transcriber), which
# runs as the NODE OWNER on the host — a different UID than the image's built-in
# `bot` (10001). Writing the WAV as 10001 left it unreadable/un-traversable by the
# node (live-prod node 1084, 2026-07-16).
#
# Fix (PUID/PGID host-UID mapping): remap the `bot` user to the node owner's
# UID/GID at boot, so every file `bot` writes into the mounts is owned by the node
# owner from the start — no node-side chown/chmod. The target UID/GID come from
# PUID/PGID (passed by citadel), else are auto-derived from the OWNER of the
# bind-mounted workspace (which IS the node owner), else fall back to the image
# default 10001. We start as root (this script, via dumb-init), remap + fix mount
# ownership, then hand off to supervisord (which drops each program to `bot`).
# Chrome refuses to run as root anyway, so a real user is required regardless.
set -e

PROFILE_DIR="${MEETING_PROFILE_DIR:-/profile}"
WORKSPACE_DIR="${CITADEL_WORKSPACE:-/workspace}"
mkdir -p "$PROFILE_DIR" "$WORKSPACE_DIR"

# Resolve the UID/GID `bot` should run as. Prefer explicit PUID/PGID; otherwise
# adopt the owner of the bind-mounted workspace (the node owner). A 0/empty result
# (root-owned or unstat-able mount) falls back to the image default 10001 — Chrome
# cannot run as root, so mapping to 0 is never useful.
TARGET_UID="${PUID:-$(stat -c '%u' "$WORKSPACE_DIR" 2>/dev/null || echo '')}"
TARGET_GID="${PGID:-$(stat -c '%g' "$WORKSPACE_DIR" 2>/dev/null || echo '')}"
[ -n "$TARGET_UID" ] && [ "$TARGET_UID" != "0" ] || TARGET_UID=10001
[ -n "$TARGET_GID" ] && [ "$TARGET_GID" != "0" ] || TARGET_GID=10001

# Remap the `bot` account to the target UID/GID (idempotent: skip when already
# correct). -o allows a non-unique id in the unlikely event of a collision with a
# baked-in account. supervisord's `user=bot` then resolves to the node owner.
if [ "$TARGET_GID" != "$(id -g bot)" ]; then
    groupmod -o -g "$TARGET_GID" bot
fi
if [ "$TARGET_UID" != "$(id -u bot)" ]; then
    usermod -o -u "$TARGET_UID" bot
fi

# The runtime/home/app dirs must be owned by the (possibly remapped) bot so pulse,
# Chrome, and uvicorn can write them. XDG_RUNTIME_DIR keeps its /run/user/10001
# path — the numeric label is cosmetic; pulse uses the env var, not a UID match.
RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/10001}"
mkdir -p "$RUNTIME_DIR/pulse"
chown -R bot:bot "$RUNTIME_DIR" /home/bot /app
chmod 700 "$RUNTIME_DIR"

# Bring the persisted profile to the target owner. This is the MIGRATION step
# for existing nodes: the profile (the signed-in Google session) was created by
# the old `bot` (10001, mode 700); after remapping `bot` to the node owner it
# would be unreadable, so chown it across. Only chown when the top dir's owner
# differs from the target (a large already-correct tree isn't re-chowned every
# boot). PROFILE_DIR is exclusively owned by this container (nothing else binds
# it), so a full recursive chown of it is safe.
if [ "$(stat -c '%u' "$PROFILE_DIR")" != "$TARGET_UID" ]; then
    chown -R bot:bot "$PROFILE_DIR"
fi

# WORKSPACE_DIR, unlike PROFILE_DIR, is a SHARED bind mount: the host `citadel
# work` process and other containerized modules (e.g. transcribe) read/write it
# directly too. NEVER chown the workspace ROOT here -- citadel-cli#551 traced a
# `chown -R` of the workspace root (reachable whenever TARGET_UID fell back to
# the image default, e.g. a root-run worker where PUID resolves to 0) to every
# host-worker file write breaking node-wide. Confine the migration chown to
# this module's OWN subdir instead: meetingd only ever writes recordings under
# "$WORKSPACE_DIR/meetings", so bring just that subdir to the target owner.
# Deliberately unconditional (unlike the PROFILE_DIR chown above, not gated on
# the parent dir's owner) so an already node-owned workspace root no longer
# hides a stale, wrong-owned meetings/ subdir left behind by a pre-#549 image
# (citadel-cli#557) -- cheap and idempotent since the dir is small.
mkdir -p "$WORKSPACE_DIR/meetings"
chown -R "$TARGET_UID:$TARGET_GID" "$WORKSPACE_DIR/meetings"

# Clear stale Chromium profile singleton locks. Chrome writes SingletonLock as
# <hostname>-<pid>; on container recreation (module upgrade, restart, `docker rm
# -f`) the hostname changes, so Chrome treats the leftover lock as "another host
# is using this profile over a network share" and REFUSES to launch, bricking every
# meeting until a human clears it. Safe to clear here: this runs before any browser
# starts, meetingd enforces one meeting at a time, and this container is the sole
# user of the bind-mounted profile.
rm -f "$PROFILE_DIR/SingletonLock" "$PROFILE_DIR/SingletonSocket" "$PROFILE_DIR/SingletonCookie" 2>/dev/null || true

exec "$@"
