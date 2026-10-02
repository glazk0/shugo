.PHONY: build test cover lint vuln run docker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/shugo ./cmd/shugo

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Recipes run in /bin/sh, so loading .env works whatever the login shell is.
run:
	@test -f .env || { echo "missing .env: cp .env.example .env and fill it in" >&2; exit 1; }
	set -a && . ./.env && set +a && go run ./cmd/shugo

docker:
	docker compose build --build-arg VERSION=$(VERSION)
