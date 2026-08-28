# Headless Hermes Agent runtime image for a Citadel node (aceteam#8170, the
# second agent-runtime harness after claudecode / citadel-cli#432). Built and
# published as ghcr.io/aceteam-ai/hermes-service (see compose.yml / the
# standalone services/compose/hermes.yml).
#
# Base image: nousresearch/hermes-agent -- the REAL, actively published,
# multi-arch (amd64/arm64) image for Nous Research's open-source Hermes Agent
# (https://github.com/NousResearch/hermes-agent, Docker Hub
# nousresearch/hermes-agent). This is not a placeholder: `docker pull
# nousresearch/hermes-agent:latest` resolves today. Pin to a digest before
# shipping to production (see the README "Build the image" section) --
# :latest is used here only to match claudecode-service's own :latest-pending
# posture (see its Dockerfile header).
#
# Unlike claudecode (which installs the official `claude` CLI into a bare
# node:22-bookworm-slim base and writes its own root-to-nonroot entrypoint),
# Hermes ships its OWN batteries-included runtime image: s6-overlay process
# supervision, a non-root `hermes` user (uid 10000) with a durable state
# volume at /opt/data, and a stage2-hook.sh that already does the chown/UID-
# remap dance claudecode's entrypoint.sh does by hand. We build ON TOP of
# that image instead of duplicating it -- see main-wrapper.sh in the upstream
# image for the privilege-drop / venv-activation logic this Dockerfile relies
# on.
FROM nousresearch/hermes-agent:latest

USER root

# The upstream image's OWN venv lives at /opt/hermes/.venv and is sealed
# read-only at runtime (HERMES_DISABLE_LAZY_INSTALLS=1, set by the base
# image) so a runtime lazy-install can't mutate Hermes's own dependency tree.
# Our wrapper's deps (fastapi/uvicorn) therefore go into a SEPARATE venv at
# /opt/wrapper-venv, built from the base image's system python3
# (/usr/bin/python3, NOT the one on PATH -- PATH resolves to Hermes's own
# venv python inside this image). This is baked at BUILD time, so the seal
# on /opt/hermes is irrelevant to it.
RUN /usr/bin/python3 -m venv /opt/wrapper-venv \
    && /opt/wrapper-venv/bin/pip install --no-cache-dir --upgrade pip
COPY requirements.txt /opt/wrapper/requirements.txt
RUN /opt/wrapper-venv/bin/pip install --no-cache-dir -r /opt/wrapper/requirements.txt

COPY wrapper.py /opt/wrapper/wrapper.py

# Fixed internal contract port for the wrapper HTTP server (the compose file
# maps the citadel-owned host port to this) -- same port number claudecode
# uses, for consistency across agent-runtime modules; there is no shared
# network namespace between them so this is not a real collision risk.
ENV PORT=8787
EXPOSE 8787

# Deliberately do NOT override ENTRYPOINT. The base image's ENTRYPOINT
# (docker/entrypoint-dispatch.sh -> /init -> main-wrapper.sh) is what
# provides the s6-overlay supervision tree AND the root-to-hermes privilege
# drop (main-wrapper.sh sources /opt/hermes/.venv/bin/activate -- putting the
# `hermes` CLI on PATH for our wrapper's subprocess calls -- then execs our
# CMD via `s6-setuidgid hermes`). main-wrapper.sh's routing rule is: "first
# arg is an executable -> exec it directly", so an absolute-path CMD like
# ours is passed straight through as the container's supervised main
# program, with the full s6 supervision tree intact.
#
# Verified locally (2026-08-25): `docker run <this image>` starts uvicorn as
# uid=10000(hermes) with `command -v hermes` resolving inside the running
# container, and GET /health returns 200 -- confirming both the privilege
# drop AND CLI-on-PATH assumptions this wrapper depends on, rather than
# taking main-wrapper.sh's behavior on faith.
CMD ["/opt/wrapper-venv/bin/uvicorn", "--app-dir", "/opt/wrapper", "wrapper:app", "--host", "0.0.0.0", "--port", "8787"]
