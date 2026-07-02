# Headless Claude Code agent-runtime image for a Citadel node
# (aceteam-ai/citadel-cli#432). Built and published as
# ghcr.io/aceteam-ai/claudecode-service (see services/compose/claudecode.yml).
#
# This is CPU-only: the model runs remotely via the AceTeam fabric proxy (pointed
# at by the ANTHROPIC_* env), so there is NO CUDA/torch/GPU here. The image
# bundles the Claude Code CLI (a Node package) plus a tiny Python/FastAPI wrapper
# (wrapper.py) that bridges AceTeam's chat routing seam to a headless `claude -p`
# turn. No secrets are baked in -- all auth is via env at run time.
#
# Base is the Node LTS slim image because the CLI is distributed on npm; we layer
# a system python3 + FastAPI/uvicorn on top for the wrapper.
FROM node:22-bookworm-slim

# System deps:
#   python3 + venv/pip  -> run the wrapper
#   git                 -> Claude Code shells out to git for repo-aware turns
#   ca-certificates     -> TLS to the fabric proxy and the platform callback
#   tini                -> proper PID 1 / signal handling for the long-lived server
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        python3 python3-venv python3-pip git ca-certificates tini \
    && rm -rf /var/lib/apt/lists/*

# Install the Claude Code CLI globally. Pin via CLAUDE_CODE_VERSION build-arg for
# reproducible images; defaults to latest. The package speaks the Anthropic
# Messages API, so pointing ANTHROPIC_BASE_URL at the fabric proxy is the whole
# trick -- no code change, no wrapper around the CLI.
ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    && npm cache clean --force

# Wrapper deps in an isolated venv (avoids Debian's PEP 668 externally-managed
# marker); put the venv on PATH so `python`/`uvicorn` resolve to it.
ENV VIRTUAL_ENV=/opt/venv
RUN python3 -m venv "$VIRTUAL_ENV"
ENV PATH="$VIRTUAL_ENV/bin:$PATH"
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY wrapper.py /app/wrapper.py

# Non-root runtime user with a real home. Claude Code state lives under
# $HOME/.claude (redirected explicitly via CLAUDE_CONFIG_DIR so it always matches
# the mounted volume regardless of $HOME resolution). The compose file bind-mounts
# a node-disk dir here so config/sessions persist across restarts.
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin claude \
    && mkdir -p /home/claude/.claude \
    && chown -R claude:claude /home/claude /app
ENV HOME=/home/claude
ENV CLAUDE_CONFIG_DIR=/home/claude/.claude

# Fixed internal contract port for the wrapper HTTP server (the compose file maps
# the citadel-owned host port to this).
ENV PORT=8787

# Keep the container fully local: no auto-update fetch, no telemetry beacon, so
# the only external traffic is to ANTHROPIC_BASE_URL (the fabric proxy) and the
# AceTeam reply callback. These honor the same knobs Claude Code documents.
ENV DISABLE_AUTOUPDATER=1
ENV CLAUDE_CODE_ENABLE_TELEMETRY=0

USER claude
WORKDIR /app
EXPOSE 8787

# tini as PID 1 for clean signal handling of the long-lived uvicorn server.
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["sh", "-c", "uvicorn wrapper:app --host 0.0.0.0 --port ${PORT}"]
