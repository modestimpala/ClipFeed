FROM python:3.11-slim
WORKDIR /app
COPY scout/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY ingestion/llm_client.py .
COPY scout/worker.py .
RUN groupadd -r appgroup && useradd -r -g appgroup appuser
USER appuser
CMD ["python", "worker.py"]
