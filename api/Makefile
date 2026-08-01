GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
BINARY_NAME=elyoraa

.PHONY: build dev test vet coverage clean lint format deps docker-build docker-up docker-down deploy-ghcr help

build:
	$(GOBUILD) -o $(BINARY_NAME) ./cmd/server

dev:
	$(GOCMD) run ./cmd/server

test:
	$(GOTEST) ./...

vet:
	$(GOCMD) vet ./...

coverage:
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f coverage.out
	rm -f coverage.html

lint:
	golangci-lint run ./... 2>/dev/null || (echo "golangci-lint not installed; falling back to go vet" && $(GOCMD) vet ./...)

format:
	gofmt -s -w .
	goimports -w . 2>/dev/null || true

deps:
	$(GOMOD) tidy

docker-build:
	docker build -t $(BINARY_NAME) -f deploy/Dockerfile .

docker-up:
	cd deploy && docker compose up -d

docker-down:
	cd deploy && docker compose down

deploy-ghcr:
	./deploy/deploy-ghcr.sh

help:
	@echo "Available commands:"
	@echo "  build        : Build the binary"
	@echo "  dev          : Run the development server"
	@echo "  test         : Run tests"
	@echo "  vet          : Run go vet"
	@echo "  coverage     : Run tests and generate HTML coverage"
	@echo "  clean        : Remove build artifacts"
	@echo "  lint         : Run linters (golangci-lint or go vet fallback)"
	@echo "  format       : Format code with gofmt and goimports"
	@echo "  deps         : Run go mod tidy"
	@echo "  docker-build : Build the Docker image"
	@echo "  docker-up    : Start docker compose"
	@echo "  docker-down  : Stop docker compose"
	@echo "  deploy-ghcr  : Build and push to GHCR"
