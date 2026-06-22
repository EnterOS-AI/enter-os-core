# Top-level Makefile — convenience wrappers around docker compose.
#
# Most molecule-core dev work happens via these shortcuts. CI doesn't
# use this Makefile; CI calls docker compose / go test directly so the
# Makefile can evolve without breaking the build.

.PHONY: help dev up down logs build test e2e-peer-visibility e2e-concierge-creates-workspace openapi-spec openapi-spec-check gen gen-docker gen-check gen-check-docker

# ─── Provider-registry SSOT codegen (internal#718) ─────────────────────
# The Go module lives in workspace-server/. The checked-in artifact
# workspace-server/internal/providers/gen/registry_gen.go is a gofmt'd
# projection of providers.yaml, drift-gated by
# .gitea/workflows/verify-providers-gen.yml. `make gen-docker` runs the SAME
# generator inside the pinned golang image so a toolchain-less env (an agent
# without Go) can regenerate without a local Go install (core#2332 follow-up).
#
# BYTE-EQUIVALENCE: gen-docker is byte-identical to native only while
# GO_VERSION below matches the `go` directive in workspace-server/go.mod.
# NOTE: the CI verify workflow pins setup-go go-version: 'stable' (not '1.25');
# that is a latent hazard — a future Go minor could reformat the artifact in CI
# vs a 1.25 local. Pin CI to '1.25' to close it (tracked alongside this change).
GO_VERSION ?= 1.25
GO_IMAGE   ?= golang:$(GO_VERSION)
DOCKER     ?= docker
# Mount the Go module (workspace-server) read-write; Go's default -mod=readonly
# keeps go.mod/go.sum untouched — only the artifact is written in-place.
DOCKER_RUN_WS = $(DOCKER) run --rm -v "$(CURDIR)/workspace-server":/src -w /src $(GO_IMAGE)

help: ## Show this help.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

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

# ─── Local prod-mimic E2E gates ────────────────────────────────────────
# Run the LITERAL peer-visibility MCP list_peers gate against the
# already-running local stack (`make up` or `make dev`). Same byte-
# identical assertion as the staging gate — only provisioning differs.
# Skips any runtime whose provider key is absent (partially-keyed env
# is fine). See tests/e2e/test_peer_visibility_mcp_local.sh for the
# env contract (CLAUDE_CODE_OAUTH_TOKEN / E2E_MINIMAX_API_KEY / etc).
e2e-peer-visibility: ## Run the LOCAL peer-visibility MCP gate vs the running stack (needs `make up` first).
	bash tests/e2e/test_peer_visibility_mcp_local.sh

# FUNCTIONAL local proof that the org concierge actually DOES org-management:
# send it a natural-language A2A request and assert it really CREATES a workspace
# via its platform MCP (create_workspace) — the deterministic side effect, not a
# REST 200. SKIPs LOUD (exit 0) unless the local concierge is seeded, online, and
# running on the platform-agent image (so create_workspace exists). To run it
# green locally: seed the concierge (MOLECULE_SEED_PLATFORM_AGENT=1) on the
# platform-agent image WITH a model key. See the script header for the contract.
e2e-concierge-creates-workspace: ## Prove the concierge actually creates a workspace via its platform MCP (skips loud if not runnable).
	bash tests/e2e/test_concierge_creates_workspace_local.sh

# ─── OpenAPI spec generation (RFC #1706, Phase 1) ─────────────────────
# Regenerate workspace-server/docs/openapi/swagger.{yaml,json} from
# swaggo annotations on the gin handlers. Commit the output. CI runs
# `make openapi-spec-check` to assert no drift between annotations and
# the committed file — if a PR changes a handler but forgets to
# regenerate, CI fails with a diff.
openapi-spec: ## Regenerate OpenAPI spec from workspace-server handler annotations.
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@v1.16.4
	cd workspace-server && swag init \
	  --generalInfo cmd/server/main.go \
	  --output docs/openapi \
	  --outputTypes yaml,json \
	  --dir . \
	  --parseDependency=false \
	  --parseInternal=true

openapi-spec-check: openapi-spec ## CI gate — fail if openapi-spec produces a diff vs the committed file.
	@git diff --exit-code -- workspace-server/docs/openapi/ \
	  || (echo "openapi-spec is stale — run 'make openapi-spec' and commit the result" && exit 1)

# ─── Provider-registry codegen targets ────────────────────────────────
gen: ## Regenerate the providers registry artifact natively (needs local Go).
	cd workspace-server && go generate ./...

gen-docker: ## Same, inside the pinned $(GO_IMAGE) — Docker only, no local Go.
	$(DOCKER_RUN_WS) go generate ./...

gen-check: ## Drift gate (native): exit 1 if the artifact is stale.
	cd workspace-server && go run ./cmd/gen-providers -check

gen-check-docker: ## Drift gate inside the pinned $(GO_IMAGE) — Docker only.
	$(DOCKER_RUN_WS) go run ./cmd/gen-providers -check
