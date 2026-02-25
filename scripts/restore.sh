#!/usr/bin/env bash
set -e

if [ -z "$1" ]; then
  echo "Usage: make restore BACKUP_DIR=backups/YYYYMMDD_HHMMSS"
  exit 1
fi

BACKUP_DIR="$1"
if [ ! -d "$BACKUP_DIR" ]; then
  echo "Backup directory $BACKUP_DIR does not exist."
  exit 1
fi

echo "Restoring SQLite database..."
docker compose stop api worker scout score-updater
docker compose cp "$BACKUP_DIR/clipfeed.db" api:/data/clipfeed.db
docker compose start api worker scout score-updater

echo "Restoring MinIO storage..."
MINIO_VOL=$(docker compose config --volumes | grep minio_data || echo "clipfeed_minio_data")
docker run --rm -v ${MINIO_VOL}:/data -v $(pwd)/$BACKUP_DIR:/backup alpine sh -c "rm -rf /data/* && tar xzf /backup/minio.tar.gz -C /data"
# Restart MinIO to pick up new files safely
docker compose restart minio

echo "Restore complete from: $BACKUP_DIR"
