# Makefile for the RunOS Cluster Agent

GREEN := \033[0;32m
RED   := \033[0;31m
BLUE  := \033[0;34m
CYAN  := \033[0;36m
GRAY  := \033[0;90m
NC    := \033[0m

GO ?= $(shell which go)

# Version is the latest git tag (sans leading v), falling back to "dev". The
# release pipeline tags the commit, so this matches the published image tag.
VERSION  := $(shell git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//' || echo "dev")
LDFLAGS  := -X github.com/runos-official/clusteragent/version.Version=$(VERSION)

# Container image. The release pipeline publishes the multiarch image here.
IMAGE_NAME := ghcr.io/runos-official/clusteragent
IMAGE_TAG  := $(VERSION)

OUT        := $(shell pwd)
HELM_FILES := $(shell find dns01/deploy -type f)

.DEFAULT_GOAL := help

# ============================================================================
# Development
# ============================================================================

# Build a local binary stamped with the version. CGO is off: the datastore now
# uses pure-Go GORM drivers, so the binary is static and cross-compiles cleanly.
.PHONY: build
build:
	@CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o clusteragent .

.PHONY: test
test:
	@echo "$(GRAY)[`date '+%H:%M:%S'`]$(NC) $(BLUE)Running tests...$(NC)"
	@$(GO) test -race ./...
	@echo "$(GRAY)[`date '+%H:%M:%S'`]$(NC) $(GREEN)Tests passed$(NC)"

.PHONY: vet
vet:
	@$(GO) vet ./...

.PHONY: clean
clean:
	@rm -f clusteragent

.PHONY: version
version:
	@echo "$(VERSION)"

# ============================================================================
# Container image (local). CI publishes via .github/workflows/release.yml.
# ============================================================================

# Build the multiarch image locally without pushing.
.PHONY: image
image:
	@docker buildx create --name multiarch-builder --use >/dev/null 2>&1 || docker buildx use multiarch-builder
	@docker buildx inspect --bootstrap >/dev/null
	@docker buildx build --platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		-t "$(IMAGE_NAME):$(IMAGE_TAG)" .

# Render the deploy manifest from the Helm chart, pinned to the current version.
.PHONY: rendered-manifest.yaml
rendered-manifest.yaml: $(OUT)/rendered-manifest.yaml

$(OUT)/rendered-manifest.yaml: $(HELM_FILES)
	@helm template runos-cluster-agent \
		--namespace runos \
		--set image.repository=$(IMAGE_NAME) \
		--set image.tag=$(IMAGE_TAG) \
		dns01/deploy > $@

# ============================================================================
# Leak gate (PUBLIC repo)
# ============================================================================

# Install the tracked git hooks for this clone. .git/hooks is not tracked, so
# every clone must do this once. Run it right after you clone.
.PHONY: hooks
hooks:
	@git config core.hooksPath .githooks
	@echo "$(GREEN)core.hooksPath = .githooks$(NC) (pre-commit now runs leakcheck on the staged diff)"

# Scan every tracked file for credentials and un-baselined internal identifiers.
.PHONY: leakcheck
leakcheck:
	@python3 scripts/leakcheck.py

# Scan only the staged diff, the same way the pre-commit hook does.
.PHONY: leakcheck-staged
leakcheck-staged:
	@python3 scripts/leakcheck.py --staged

# Ratchet the baseline down after you REMOVE an identifier from the source.
# Never run this to get a new identifier past the gate.
.PHONY: leakcheck-update
leakcheck-update:
	@python3 scripts/leakcheck.py --update

# Test the checker itself: what it must catch and what it must not.
.PHONY: leakcheck-test
leakcheck-test:
	@python3 scripts/leakcheck_test.py

# ============================================================================
# Release
# ============================================================================

# Cut a release: gates (build/vet/test) + sensitivity floor, tag, push, watch
# CI, verify the image attestation. Usage: make release RELEASE_VERSION=v0.18.0
# (add CHECK=1 to run gates only, no tag/push).
.PHONY: release
release:
	@test -n "$(RELEASE_VERSION)" || { echo "$(RED)set RELEASE_VERSION, e.g. make release RELEASE_VERSION=v0.18.0$(NC)"; exit 1; }
	@scripts/release.sh $(RELEASE_VERSION) $(if $(CHECK),--check,)

# ============================================================================
# Help
# ============================================================================

.PHONY: help
help:
	@echo "$(CYAN)RunOS Cluster Agent$(NC)"
	@echo ""
	@echo "  make build      Build a local binary stamped with the version"
	@echo "  make test       Run tests with the race detector"
	@echo "  make vet        Run go vet"
	@echo "  make version    Show the current version"
	@echo "  make clean      Remove build artifacts"
	@echo ""
	@echo "  make image                       Build the multiarch image locally (no push)"
	@echo "  make rendered-manifest.yaml      Render the deploy manifest from the Helm chart"
	@echo ""
	@echo "  make hooks            Install the tracked git hooks (run once per clone)"
	@echo "  make leakcheck        Scan every tracked file for leaks (PUBLIC repo gate)"
	@echo "  make leakcheck-staged Scan only the staged diff"
	@echo "  make leakcheck-update Ratchet the baseline down after removing an identifier"
	@echo "  make leakcheck-test   Test the leak checker itself"
	@echo ""
	@echo "  make release RELEASE_VERSION=vX.Y.Z          Cut a release (gates, tag, push, verify)"
	@echo "  make release RELEASE_VERSION=vX.Y.Z CHECK=1  Run release gates only, no tag/push"
