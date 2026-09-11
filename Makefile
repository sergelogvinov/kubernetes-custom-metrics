#
REGISTRY ?= ghcr.io
USERNAME ?= sergelogvinov
OCIREPO  ?= $(REGISTRY)/$(USERNAME)
HELMREPO ?= $(REGISTRY)/$(USERNAME)/charts
PLATFORM ?= linux/arm64,linux/amd64
PUSH ?= false

SHA ?= $(shell git describe --match=none --always --abbrev=7 --dirty)
TAG ?= $(shell git describe --tag --always --match v[0-9]\*)

GO_LDFLAGS  := -ldflags "-w -s -X main.version=$(TAG) -X main.commit=$(SHA)"
GOOS        ?= $(shell go env GOOS)
GOARCH      ?= $(shell go env GOARCH)

############

# Help Menu

define HELP_MENU_HEADER
# Getting Started

To build this project, you must have the following installed:

- git
- make
- golang 1.26+
- golangci-lint

endef

export HELP_MENU_HEADER

.PHONY: help
help: ## This help menu
	@echo "$$HELP_MENU_HEADER"
	@grep -E '^[a-zA-Z0-9%_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

############
#
# Build Abstractions
#

.PHONY: all
all: build

.PHONY: clean
clean: ## Clean
	@rm -rf bin/ kubectl-btop custom-metrics

.PHONY: tools
tools: ## Install necessary development tools
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
	go install github.com/google/go-licenses@latest

.PHONY: build
build: ## Build
	@mkdir -p bin/
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_LDFLAGS) \
	-o bin/kubectl-btop ./cmd/kubectl-btop
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_LDFLAGS) \
	-o bin/custom-metrics ./cmd/custom-metrics

.PHONY: generate
generate: ## Run controller-gen and other generators
	controller-gen rbac:roleName=custom-metrics \
		paths=./cmd/custom-metrics \
		output:rbac:artifacts:config=deploy/base

.PHONY: manifests
manifests: ## Validate deployment YAML and RBAC policy
	@test -f deploy/base/role.yaml || { echo "missing deploy/base/role.yaml; run make generate"; exit 1; }
	@go run sigs.k8s.io/yaml@latest 2>/dev/null || true
	@echo "manifests: deploy/base/role.yaml present"

############
#
# Test Abstractions
#

.PHONY: lint
lint: ## Lint Code
	golangci-lint run --config .golangci.yml

.PHONY: vet
vet: ## Vet code
	go vet ./...

.PHONY: test
test: ## Run all tests with the race detector
	go test -race -count=1 ./...

.PHONY: unit
unit: lint test ## Run unit tests

.PHONY: test-rules
test-rules: ## Run promtool against normalized recording-rule fixtures
	@command -v promtool >/dev/null 2>&1 || { echo "promtool not found; install prometheus tooling"; exit 1; }
	promtool check rules monitoring/rules.yaml
	promtool test rules monitoring/rules_test.yaml

.PHONY: licenses
licenses: ## Check licenses of all dependencies
	go-licenses check ./... --disallowed_types=forbidden,restricted,unknown
