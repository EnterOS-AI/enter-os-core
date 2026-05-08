# Top-level Makefile — convenience wrappers around docker compose.
#
# Most molecule-core dev work happens via these shortcuts. CI doesn't
# use this Makefile; CI calls docker compose / go test directly so the
# Makefile can evolve without breaking the build.

.PHONY: help dev up down logs build test

help: ## Show this help.
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'

dev: ## Start the full stack with air hot-reload for the platform service.
	docker compose -f docker-compose.yml -f docker-compose.dev.yml up

up: ## Start the full stack in production-shape mode (no air, normal Dockerfile).
	docker compose up

down: ## Stop the stack and remove containers (volumes preserved).
	docker compose down

logs: ## Tail logs from all services (Ctrl-C to detach).
	docker compose logs -f

build: ## Force a fresh build of the platform image (no cache).
	docker compose build --no-cache platform

test: ## Run Go unit tests in workspace-server/.
	cd workspace-server && go test -race ./...
