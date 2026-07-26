COMPOSE_FILE := deploy/compose.yml

.PHONY: compose-up compose-down migrate run client-dev test smoke run-all stop-all

compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

compose-down:
	docker compose -f $(COMPOSE_FILE) down

migrate:
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql -ufarm -pfarm farm < server/migrations/001_init.sql

run:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/farm-server

client-dev:
	cd client && npm run dev

test:
	cd server && go test ./...

smoke:
	@set -a; \
	if [ -f .env ]; then . ./.env; fi; \
	set +a; \
	cd server && go run ./cmd/smoke

run-all:
	./scripts/run.sh

stop-all:
	./scripts/stop.sh
