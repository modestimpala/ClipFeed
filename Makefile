.PHONY: up down build logs shell-api shell-worker shell-db lifecycle score test-api-docker clean

up:
	docker compose up -d

down:
	docker compose down

build:
	docker compose build --no-cache

logs:
	docker compose logs -f

logs-worker:
	docker compose logs -f worker

logs-api:
	docker compose logs -f api

shell-api:
	docker compose exec api sh

shell-worker:
	docker compose exec worker bash

shell-db:
	docker compose exec api sqlite3 /data/clipfeed.db

lifecycle:
	docker compose exec worker python lifecycle.py

score:
	docker compose exec score-updater python score_updater.py

test-api-docker:
	docker run --rm -v $(PWD)/api:/src -w /src golang:1.24 sh -lc 'go test ./...'

dev-api:
	cd api && go run .

dev-web:
	cd web && npm run dev

clean:
	docker compose down -v --remove-orphans
