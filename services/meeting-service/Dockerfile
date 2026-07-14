# Citadel meeting media stack image (aceteam-ai/citadel-cli#514). Built and
# published as ghcr.io/aceteam-ai/meeting-service (see the build workflow and
# services/meeting-service/compose.yml).
#
# This packages the meeting notetaker's media layer -- Chromium + Xvfb +
# PulseAudio null sink + ffmpeg -- plus a small session supervisor (meetingd) into
# ONE long-lived Linux container. Audio is fully in-container: a PulseAudio null
# sink captures only the browser's own streams (PULSE_SINK), so there is NO host
# audio dependency, NO /dev/snd, and NO host PulseAudio/Xvfb/Chromium needed. That
# is what lets this run identically on a bare Linux box and inside Docker Desktop
# on macOS/Windows.
#
# Deliberate design note: the design brief named "pipewire-pulse"; this image uses
# plain PulseAudio. The two are interchangeable for everything this stack depends
# on (the pulse socket, pactl, PULSE_SINK routing, and `ffmpeg -f pulse -i
# <sink>.monitor`), and the host meeting code already treats them as equivalent
# ("PulseAudio/pipewire-pulse server"). PulseAudio is the proven, simplest headless
# null-sink capture path for the #1 historical risk here (silent audio), so it is
# the lower-risk choice for the vertical slice.
FROM debian:bookworm-slim

# System deps:
#   chromium                 -> the meeting browser (headed, on Xvfb)
#   xvfb                     -> virtual display :99 (no GPU; chrome uses softwareGL)
#   pulseaudio + utils       -> the audio server + pactl/paplay for the null sink
#   ffmpeg                   -> records the sink monitor to mono/16 kHz WAV; also
#                               generates the health-check canary tone
#   fonts-*                  -> so Meet renders (missing fonts break layout/detection)
#   python3 + venv + pip     -> run meetingd (FastAPI/uvicorn)
#   supervisor               -> supervise Xvfb + pulse + meetingd in one container
#   dumb-init                -> clean PID 1 / signal reaping for the long-lived stack
#   gosu                     -> (available for entrypoint privilege drops if needed)
#   procps, ca-certificates  -> ps for process mgmt; TLS roots for Meet
#
# Chromium is pinned to whatever the Debian bookworm `chromium` package resolves to
# at build time. That is fine for audio capture (version-independent). It matters
# only for the signed-in profile path (a later PR): Chrome refuses a profile written
# by a NEWER Chrome, so a host-seeded profile must be seeded with a Chrome no newer
# than this image's. The seeding doc documents that constraint; the durable fix is
# in-container seeding (follow-up).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        chromium \
        xvfb \
        pulseaudio pulseaudio-utils \
        ffmpeg \
        fonts-liberation fonts-noto-core fonts-noto-color-emoji \
        python3 python3-venv python3-pip \
        supervisor \
        socat \
        dumb-init gosu procps ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# meetingd deps in an isolated venv (avoids Debian's PEP 668 externally-managed
# marker); put the venv on PATH so python/uvicorn resolve to it.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY meetingd.py /app/meetingd.py
COPY supervisord.conf /etc/supervisor/supervisord.conf
COPY citadel.pa /etc/pulse/citadel.pa
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Non-root runtime user with a real home + a pulse runtime dir. The compose file
# bind-mounts host dirs onto /profile and /workspace; the entrypoint chowns them to
# `bot` at boot so a fresh host-owned mount "just works" with no manual pre-create.
RUN useradd --create-home --uid 10001 --shell /bin/bash bot \
    && mkdir -p /workspace /profile /run/user/10001/pulse /home/bot/.config/pulse \
    && chown -R bot:bot /home/bot /run/user/10001 /workspace /profile /app \
    && chmod +x /usr/local/bin/entrypoint.sh

# meetingd control API (loopback contract port) and the Chromium CDP port. The
# compose file maps citadel-owned host ports to these; both are published to
# 127.0.0.1 only.
ENV MEETINGD_PORT=8102
# CDP is published on 9223, but Chromium binds its DevTools socket to 127.0.0.1
# ONLY (modern Chromium ignores --remote-debugging-address for a non-loopback bind
# as a DNS-rebinding mitigation), which a docker port publish cannot reach. So
# Chromium listens on the loopback-internal 9222 and a socat forwarder
# (supervisord) bridges the container-external 9223 -> 127.0.0.1:9222. This keeps
# Chromium's launch flags identical to the host builder (loopback CDP) while still
# publishing the port.
ENV MEETING_CDP_PORT=9223
ENV MEETING_CDP_INTERNAL_PORT=9222
ENV XVFB_RESOLUTION=1280x720x24
ENV DISPLAY=:99
ENV HOME=/home/bot
ENV XDG_RUNTIME_DIR=/run/user/10001
ENV PULSE_SERVER=unix:/run/user/10001/pulse/native
ENV MEETING_PROFILE_DIR=/profile
ENV CITADEL_WORKSPACE=/workspace

EXPOSE 8102 9223

# dumb-init as PID 1 for signal handling; entrypoint fixes mount ownership then
# execs supervisord (which drops each program to the bot user).
ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf"]
