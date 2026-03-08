## ── kgpudash Makefile ─────────────────────────────────────────────────────
##
## Usage:
##   make build          Build both binaries locally
##   make docker-build   Build Docker images
##   make docker-push    Push Docker images to registry
##   make deploy         Apply all Kubernetes manifests
##   make undeploy       Remove all Kubernetes resources
##   make proto          Regenerate protobuf Go code
##   make tidy           Run go mod tidy
##   make lint           Run golangci-lint
##   make test           Run unit tests

# ── Configuration ──────────────────────────────────────────────────────────
REGISTRY   ?= ghcr.io/michaeltrip
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
AGENT_IMG  := $(REGISTRY)/kgpudash-agent:$(VERSION)
SERVER_IMG := $(REGISTRY)/kgpudash-server:$(VERSION)

GOFLAGS    := -ldflags="-s -w -X main.version=$(VERSION)"

# ── Build ──────────────────────────────────────────────────────────────────
.PHONY: build
build: build-agent build-server

.PHONY: build-agent
build-agent:
	CGO_ENABLED=1 go build $(GOFLAGS) -o bin/kgpudash-agent ./cmd/agent

.PHONY: build-server
build-server:
	CGO_ENABLED=0 go build $(GOFLAGS) -o bin/kgpudash-server ./cmd/server

# ── Docker ─────────────────────────────────────────────────────────────────
.PHONY: docker-build
docker-build: docker-build-agent docker-build-server

.PHONY: docker-build-agent
docker-build-agent:
	docker build -f Dockerfile.agent -t $(AGENT_IMG) .
	docker tag $(AGENT_IMG) $(REGISTRY)/kgpudash-agent:latest

.PHONY: docker-build-server
docker-build-server:
	docker build -f Dockerfile.server -t $(SERVER_IMG) .
	docker tag $(SERVER_IMG) $(REGISTRY)/kgpudash-server:latest

.PHONY: docker-push
docker-push:
	docker push $(AGENT_IMG)
	docker push $(REGISTRY)/kgpudash-agent:latest
	docker push $(SERVER_IMG)
	docker push $(REGISTRY)/kgpudash-server:latest

# ── Kubernetes ─────────────────────────────────────────────────────────────
.PHONY: deploy
deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/agent-daemonset.yaml
	kubectl apply -f deploy/server-deployment.yaml
	kubectl apply -f deploy/service.yaml

.PHONY: undeploy
undeploy:
	kubectl delete -f deploy/service.yaml          --ignore-not-found
	kubectl delete -f deploy/server-deployment.yaml --ignore-not-found
	kubectl delete -f deploy/agent-daemonset.yaml   --ignore-not-found
	kubectl delete -f deploy/rbac.yaml              --ignore-not-found

.PHONY: status
status:
	kubectl -n kgpudash get pods,svc,daemonset,deployment

# ── Protobuf ───────────────────────────────────────────────────────────────
.PHONY: proto
proto:
	PATH="$$PATH:$$(go env GOPATH)/bin" protoc \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  internal/proto/gpu/gpu.proto

# ── Go tooling ─────────────────────────────────────────────────────────────
.PHONY: tidy
tidy:
	go mod tidy

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: test
test:
	go test -race -count=1 ./...

.PHONY: vet
vet:
	go vet ./...

# ── Port-forward (dev convenience) ────────────────────────────────────────
.PHONY: port-forward
port-forward:
	kubectl -n kgpudash port-forward svc/kgpudash-server 8080:80

# ── Clean ──────────────────────────────────────────────────────────────────
.PHONY: clean
clean:
	rm -rf bin/

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
