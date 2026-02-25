.PHONY: up down build logs shell-api shell-worker shell-db lifecycle score test-api-docker clean \
       ai-up ai-down gpu-up gpu-down gpu-build gpu-logs gpu-logs-worker gpu-logs-api

GPU_COMPOSE := docker compose -f docker-compose.yml -f docker-compose.gpu.yml --profile ai

up:
	docker compose up -d

down:
	docker compose down

ai-up:
	docker compose --profile ai up -d

ai-down:
	docker compose --profile ai down

build:
	docker compose build --no-cache

logs:
	docker compose logs -f

logs-worker:
	docker compose logs -f worker

logs-api:
	docker compose logs -f api

gpu-up:
	$(GPU_COMPOSE) up -d

gpu-down:
	$(GPU_COMPOSE) down

gpu-build:
	$(GPU_COMPOSE) build --no-cache

gpu-logs:
	$(GPU_COMPOSE) logs -f

gpu-logs-worker:
	$(GPU_COMPOSE) logs -f worker

gpu-logs-api:
	$(GPU_COMPOSE) logs -f api

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

clean:
	docker compose down -v --remove-orphans
