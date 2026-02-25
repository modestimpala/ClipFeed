.PHONY: up down build logs logs-worker logs-api \
       shell-api shell-worker shell-db lifecycle score test-api-docker \
       dev-api dev-web backup restore clean

# All `docker compose` commands automatically read COMPOSE_PROFILES and
# COMPOSE_FILE from .env, so there is no need for per-combo targets.
#
# Examples for .env:
#   Base stack (no AI)  → leave COMPOSE_PROFILES empty
#   AI + cloud LLM      → COMPOSE_PROFILES=ai
#   AI + local Ollama    → COMPOSE_PROFILES=ai,ollama
#   GPU + cloud LLM      → COMPOSE_PROFILES=ai  +  COMPOSE_FILE=docker-compose.yml:docker-compose.gpu.yml
#   GPU + local Ollama   → COMPOSE_PROFILES=ai,ollama  +  COMPOSE_FILE=docker-compose.yml:docker-compose.gpu.yml

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
	docker run --rm \
		-e GOMODCACHE=/go/pkg/mod \
		-e GOCACHE=/root/.cache/go-build \
		-v $(PWD)/api:/src \
		-v $(PWD)/.cache/go-mod:/go/pkg/mod \
		-v $(PWD)/.cache/go-build:/root/.cache/go-build \
		-w /src golang:1.24-alpine sh -c 'go mod tidy && go test ./... 2>&1 | cat'

dev-api:
	cd api && go run .

dev-web:
	cd web && npm run dev

backup:
	./scripts/backup.sh

restore:
	./scripts/restore.sh $(BACKUP_DIR)

clean:
	docker compose down -v --remove-orphans
