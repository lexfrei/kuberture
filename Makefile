BINARY := kuberture
IMAGE  := ghcr.io/lexfrei/kuberture

.PHONY: build test lint lint-fix fmt vet tidy image helm-lint helm-package e2e all

build:
	go build -ldflags="-s -w" -o bin/$(BINARY) ./cmd/kuberture

test:
	go test ./... -v -race -coverprofile=coverage.out

lint:
	golangci-lint run

lint-fix:
	golangci-lint run --fix

fmt:
	gofumpt -w .
	goimports -w .

vet:
	go vet ./...

tidy:
	go mod tidy

image:
	docker build -t $(IMAGE):dev -f Containerfile .

helm-lint:
	helm lint chart/

helm-package:
	helm package chart/

e2e:
	bash e2e/run.sh

all: fmt vet lint test build
