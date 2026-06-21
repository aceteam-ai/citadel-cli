# Node-local faster-whisper STT sidecar for Citadel.
# Built and published as ghcr.io/aceteam-ai/whisper-service (see transcribe.yml).
FROM python:3.12-slim

# ffmpeg is required by faster-whisper to decode webm/mp3/wav inputs.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py /app/app.py

ENV PORT=8101
EXPOSE 8101

CMD ["sh", "-c", "uvicorn app:app --host 0.0.0.0 --port ${PORT}"]
