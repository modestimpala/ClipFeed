FROM python:3.11-slim
WORKDIR /app
COPY scout/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY ingestion/llm_client.py .
COPY scout/worker.py .
CMD ["python", "worker.py"]
