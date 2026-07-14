#!/bin/sh
# Entrypoint for the Citadel meeting media stack (aceteam-ai/citadel-cli#514).
#
# The container runs Xvfb, PulseAudio, meetingd, and per-session Chromium/ffmpeg
# as the non-root `bot` user (uid 10001). Two dirs are bind-mounted from the host
# (the signed-in Chrome profile at /profile and the shared node workspace at
# /workspace); a bind-mount source is owned by whoever created it on the host, NOT
# 10001, so without a fixup the bot user cannot write the profile or the WAV.
#
# Fix: start as root (this script, PID via dumb-init), ensure the pulse runtime
# dir and the mounts are bot-owned, then hand off to supervisord (which drops each
# program to `bot`). Chrome refuses to run as root anyway, so a real user is
# required regardless.
set -e

RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/10001}"
mkdir -p "$RUNTIME_DIR/pulse"
chown -R bot:bot "$RUNTIME_DIR"
chmod 700 "$RUNTIME_DIR"

# Only chown the mounts when needed (a large persisted profile shouldn't be
# re-chowned top-to-bottom every boot). If the top dir is already bot-owned,
# assume the tree is fine.
for dir in "${MEETING_PROFILE_DIR:-/profile}" "${CITADEL_WORKSPACE:-/workspace}"; do
    mkdir -p "$dir"
    if [ "$(stat -c '%u' "$dir")" != "10001" ]; then
        chown -R bot:bot "$dir"
    fi
done

# Clear stale Chromium profile singleton locks. Chrome writes SingletonLock as
# <hostname>-<pid>; on container recreation (module upgrade, restart, `docker rm
# -f`) the hostname changes, so Chrome treats the leftover lock as "another host
# is using this profile over a network share" and REFUSES to launch, bricking every
# meeting until a human clears it. Safe to clear here: this runs before any browser
# starts, meetingd enforces one meeting at a time, and this container is the sole
# user of the bind-mounted profile.
PROFILE="${MEETING_PROFILE_DIR:-/profile}"
rm -f "$PROFILE/SingletonLock" "$PROFILE/SingletonSocket" "$PROFILE/SingletonCookie" 2>/dev/null || true

exec "$@"
