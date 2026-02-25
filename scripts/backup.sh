#!/usr/bin/env bash
set -e

BACKUP_DIR="backups/$(date +%Y%m%d_%H%M%S)"
echo "Creating backup in $BACKUP_DIR..."
mkdir -p "$BACKUP_DIR"

echo "Backing up SQLite database..."
docker compose exec -T api sqlite3 /data/clipfeed.db ".backup '/tmp/backup.db'"
docker compose cp api:/tmp/backup.db "$BACKUP_DIR/clipfeed.db"
docker compose exec -T api rm /tmp/backup.db

echo "Backing up MinIO storage..."
MINIO_VOL=$(docker compose config --volumes | grep minio_data || echo "clipfeed_minio_data")
docker run --rm -v ${MINIO_VOL}:/data -v $(pwd)/$BACKUP_DIR:/backup alpine tar czf /backup/minio.tar.gz -C /data .

echo "Backup complete: $BACKUP_DIR"
