.PHONY: build build-meshctl build-agent release test lint clean

BINARY_DIR := bin
GOFLAGS := -trimpath

build: build-meshctl build-agent

build-meshctl:
	go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl ./cmd/meshctl

build-agent:
	go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl-agent ./cmd/meshctl-agent

release:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl-linux-amd64 ./cmd/meshctl
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl-linux-arm64 ./cmd/meshctl
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl-agent-linux-amd64 ./cmd/meshctl-agent
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o $(BINARY_DIR)/meshctl-agent-linux-arm64 ./cmd/meshctl-agent

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -rf $(BINARY_DIR)
