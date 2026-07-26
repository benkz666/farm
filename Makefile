COMPOSE_FILE := deploy/compose.yml

.PHONY: compose-up compose-down migrate run client-dev test smoke

compose-up:
	docker compose -f $(COMPOSE_FILE) up -d

compose-down:
	docker compose -f $(COMPOSE_FILE) down

migrate:
	@echo TODO: run server migrations

run:
	@echo TODO: start farm-server

client-dev:
	cd client && npm run dev

test:
	@echo TODO: go test ./...

smoke:
	@echo TODO: run smoke checks
