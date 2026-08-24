GOCACHE ?= $(HOME)/.cache/go-build
GOPATH  ?= $(HOME)/.cache/go
export GOCACHE GOPATH

# 容器化（alpine base，latest tag）
IMAGE_NAME ?= slo-sentinel
IMAGE_TAG  ?= latest
DOCKER     ?= docker

.PHONY: build test vet lint promtool-check clean docker-build docker-up docker-down docker-logs

build:
	go build -o bin/sentinel ./cmd/sentinel
	go build -o bin/sentinel-ui ./cmd/sentinel-ui
	go build -o bin/sentinel-gen ./cmd/sentinel-gen

test:
	go test ./...

vet: 
	go vet ./...

lint: vet
	@command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint 未安裝，略過"

promtool-check:
	@command -v promtool >/dev/null && promtool check rules rules.d/*.yaml || echo "promtool 未安裝或無規則檔，略過"

clean:
	rm -rf bin

docker-build: ## 建置映像（多階段：golang:alpine → alpine:latest）
	$(DOCKER) build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-up: ## 啟動 daemon + UI（docker compose up -d）
	$(DOCKER) compose up -d --build

docker-down: ## 停止並移除容器（保留資料卷）
	$(DOCKER) compose down

docker-logs: ## 追蹤兩個服務的日誌
	$(DOCKER) compose logs -f
