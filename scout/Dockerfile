FROM python:3.11-slim
WORKDIR /app
RUN pip install --no-cache-dir requests yt-dlp
COPY ingestion/ollama_client.py .
COPY scout/worker.py .
CMD ["python", "worker.py"]
