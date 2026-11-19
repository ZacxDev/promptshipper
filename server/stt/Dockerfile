# promptshipper STT server — CPU image (default backend: faster-whisper).
#
# No torch, no CUDA: faster-whisper runs on CTranslate2. The model is NOT baked
# in; it downloads on first start into $HF_HOME, which compose mounts as a
# volume so it survives restarts and upgrades.
FROM python:3.12-slim

# 🔴 ffmpeg IS A HARD RUNTIME DEPENDENCY: app.py shells out to it for every
# request (decode_audio). Without it every transcription is a 503.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py check_tokens.py ./
COPY backends/ ./backends/

# Non-root. /models and /etc/snip exist in the image, owned by the runtime user,
# because Docker seeds a NEW named volume from the image directory at its mount
# point — ownership included — so the model download can write there.
RUN groupadd --system --gid 10001 stt \
    && useradd --system --uid 10001 --gid 10001 --home-dir /app stt \
    && mkdir -p /models/hf /etc/snip \
    && chown -R 10001:10001 /models /etc/snip
USER 10001
ENV HF_HOME=/models/hf \
    STT_BACKEND=faster-whisper \
    PYTHONUNBUFFERED=1

EXPOSE 8000
CMD ["python", "-m", "uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
