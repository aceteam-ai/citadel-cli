# Node-local whisperx STT + speaker-diarization sidecar for Citadel.
# Built and published as ghcr.io/aceteam-ai/whisper-service (see transcribe.yml).
#
# CPU image: torch/torchaudio are installed from the CPU wheel index to keep the
# image lean and dodge CUDA (cuDNN/ctranslate2) version drift. The service
# auto-detects device at runtime, so a future CUDA base image needs no code
# change -- only a GPU torch build and `deploy.resources` GPU reservation.
FROM python:3.12-slim

# ffmpeg is required to decode webm/mp3/wav inputs (whisperx.load_audio).
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install CPU torch first so whisperx's transitive torch requirement resolves
# to the lean CPU wheel instead of the multi-GB CUDA default. Versions match
# whisperx 3.8.6's pins (torch~=2.8.0, torchvision~=0.23.0) so pip does not
# re-resolve them to the CUDA build when whisperx is installed next.
RUN pip install --no-cache-dir \
    torch==2.8.0 torchaudio==2.8.0 torchvision==0.23.0 \
    --index-url https://download.pytorch.org/whl/cpu

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py /app/app.py

ENV PORT=8101
EXPOSE 8101

CMD ["sh", "-c", "uvicorn app:app --host 0.0.0.0 --port ${PORT}"]
