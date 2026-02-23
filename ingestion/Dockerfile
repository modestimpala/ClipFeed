FROM python:3.11-slim

ARG ENABLE_GPU=false

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg curl nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install -r requirements.txt

# CUDA libraries for GPU inference (~700 MB); skipped for CPU-only builds
RUN if [ "$ENABLE_GPU" = "true" ]; then \
    pip install --no-cache-dir nvidia-cublas-cu12 nvidia-cudnn-cu12; \
    fi

# Bake base whisper model into image (avoids cold-start download)
RUN python -c "from faster_whisper import WhisperModel; WhisperModel('base', device='cpu', compute_type='int8')"

COPY worker.py lifecycle.py score_updater.py ./

ENV PYTHONUNBUFFERED=1
CMD ["python", "worker.py"]
