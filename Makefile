GOCACHE ?= $(HOME)/.cache/go-build
GOPATH  ?= $(HOME)/.cache/go
export GOCACHE GOPATH

.PHONY: build test vet lint promtool-check clean

build:
	go build -o bin/sentinel ./cmd/sentinel
	go build -o bin/sentinel-ui ./cmd/sentinel-ui

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
