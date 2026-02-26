FROM python:3.11-slim

ARG ENABLE_GPU=false

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg curl nodejs \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .

# CPU-only builds use the PyTorch CPU index (~300 MB vs ~2.4 GB for CUDA wheels)
RUN --mount=type=cache,target=/root/.cache/pip \
    if [ "$ENABLE_GPU" = "true" ]; then \
        pip install -r requirements.txt && \
        pip install --no-cache-dir nvidia-cublas-cu12 nvidia-cudnn-cu12; \
    else \
        pip install -r requirements.txt \
            --extra-index-url https://download.pytorch.org/whl/cpu; \
    fi

# Bake base whisper model into image (avoids cold-start download)
RUN python -c "from faster_whisper import WhisperModel; WhisperModel('base', device='cpu', compute_type='int8')"

COPY worker.py lifecycle.py score_updater.py llm_client.py api_client.py ./
COPY l2r/ ./l2r/

RUN groupadd -r -g 1000 appgroup && useradd -r -u 1000 -g appgroup -m appuser
RUN mkdir -p /tmp/clipfeed && chown appuser:appgroup /tmp/clipfeed
ENV PYTHONUNBUFFERED=1 HF_HOME=/home/appuser/.cache/huggingface
USER appuser
CMD ["python", "worker.py"]
