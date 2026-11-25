SHELL := /bin/bash
export PATH := $(HOME)/.local/go/bin:$(HOME)/go/bin:$(PATH)

DATABASE_URL ?= postgres://logbook:logbook@127.0.0.1:54329/logbook?sslmode=disable
export DATABASE_URL

.PHONY: check fmt lint vet test vuln generate db-up db-down migrate build run web-check web-deploy docs-dev docs-build docs-check

check: fmt vet lint test vuln ## everything CI runs

fmt:
	@set -o pipefail; test -z "$$(gofmt -l cmd internal pkg)" || (gofmt -l cmd internal pkg; echo "run gofmt -w"; exit 1)

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test:
	go test -race -count=1 -timeout 20m ./...

vuln:
	govulncheck ./...

generate:
	sqlc generate

db-up:
	docker compose -f deploy/compose.dev.yml up -d --wait

db-down:
	docker compose -f deploy/compose.dev.yml down

migrate:
	goose -dir migrations postgres "$(DATABASE_URL)" up

build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

run: build
	./bin/logbook-server

web-check: ## the web UI's checks (types, lint, unit tests, production build)
	cd web && bun install --frozen-lockfile && bun run check && bun run lint && bun run test && \
		LOGBOOK_API_URL=https://api.logbook.boogles.dev bun run build

web-deploy: web-check ## upload the web UI to Cloudflare Pages (needs CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID)
	cd web && bunx wrangler pages deploy build --project-name logbook --branch main \
		--commit-hash "$$(git rev-parse HEAD)" --commit-message "$$(git log -1 --format=%s)"

docs-dev: ## the docs site at http://localhost:4321, reloading as you edit
	cd site && pnpm install --frozen-lockfile && pnpm dev

docs-build: ## the docs site's production build, into site/dist
	cd site && pnpm install --frozen-lockfile && pnpm build

# The link validator runs inside every build, so docs-check is a full build that
# must pass: it fails on a broken internal link or #anchor, a page missing from its
# topic's sidebar, or a <Shot> whose screenshot is missing or has no alt text.
docs-check: docs-build ## build the docs with link, sidebar and screenshot checks
	@test -f site/dist/pagefind/pagefind.js || (echo "docs-check: search index missing from site/dist"; exit 1)
	@echo "docs-check: ok"
