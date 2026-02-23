.PHONY: up down build logs shell-api shell-worker shell-db lifecycle score migrate clean

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
	docker compose exec postgres psql -U clipfeed clipfeed

lifecycle:
	docker compose exec worker python lifecycle.py

score:
	docker compose exec score-updater python score_updater.py

migrate:
	docker compose exec postgres psql -U clipfeed clipfeed \
	  -f /docker-entrypoint-initdb.d/002_score_function.sql \
	  -f /docker-entrypoint-initdb.d/003_platform_cookies.sql

dev-api:
	cd api && go run .

dev-web:
	cd web && npm run dev

clean:
	docker compose down -v --remove-orphans
