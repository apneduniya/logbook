SHELL := /bin/bash
export PATH := $(HOME)/.local/go/bin:$(HOME)/go/bin:$(PATH)

DATABASE_URL ?= postgres://logbook:logbook@127.0.0.1:54329/logbook?sslmode=disable
export DATABASE_URL

.PHONY: check fmt lint vet test vuln generate db-up db-down migrate build run

check: fmt vet lint test vuln ## everything CI runs

fmt:
	@set -o pipefail; test -z "$$(gofmt -l cmd internal pkg)" || (gofmt -l cmd internal pkg; echo "run gofmt -w"; exit 1)

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test:
	go test -race -count=1 ./...

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
