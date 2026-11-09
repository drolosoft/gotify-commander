PLUGIN_NAME := gotify-commander
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# The Gotify release the plugin is built for. go.mod is pinned to one release
# at a time (scripts/pin-gotify.sh writes .gotify-version and .go-version);
# asking for another one re-pins go.mod first.
GOTIFY_VERSION ?= $(shell cat .gotify-version)
ARCH ?= amd64

# The release matrix of build-all. A plugin only loads on the exact server
# release it was built for, so every release we support gets its own .so.
GOTIFY_VERSIONS := 2.9.1 3.1.1
ARCHES := amd64 arm64 arm-7

BUILD_DIR := build

# The plugin reports VERSION on the Gotify plugin page and in the User-Agent
# of locate. It is computed here, from the local checkout.
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

# gotify/build images exist only for linux/amd64: each tag is a cross
# compiler with GOARCH and CC already set for its target, so on an arm64 Mac
# every build runs under emulation. The Go caches live in named volumes and
# not in the host's ~/go, because a module cache filled by another Go version
# is not safe to share. GOTOOLCHAIN=local makes the image's Go build the
# plugin even if go.mod would ask for another one.
DOCKER_RUN := docker run --rm --platform linux/amd64 \
	-e GOTOOLCHAIN=local -e GOFLAGS=-mod=readonly \
	-v gotify-commander-gocache:/root/.cache/go-build \
	-v gotify-commander-gomod:/go/pkg/mod \
	-v "$$PWD:/proj" -w /proj

# -buildvcs=false: the version comes from LDFLAGS, and the container cannot
# read the .git of a worktree, whose real directory lives outside /proj.
DOCKER_GO_BUILD := go build -buildvcs=false -a -installsuffix cgo -buildmode=plugin $(LDFLAGS)

.PHONY: test lint build pin build-plugin build-all preview clean help

test:
	go test ./... -v -count=1

lint:
	go vet ./...

build: test lint
	@echo "✅ Tests and lint passed (the plugin .so is built with make build-plugin)"

pin:
	scripts/pin-gotify.sh $(GOTIFY_VERSION)

# The Go version and the image are read inside the recipe, after a possible
# re-pin, because .go-version may change between Gotify releases.
build-plugin:
	@if [ "$(GOTIFY_VERSION)" != "$$(cat .gotify-version)" ]; then \
		scripts/pin-gotify.sh $(GOTIFY_VERSION); \
	fi
	@mkdir -p $(BUILD_DIR)
	@echo "🔨 Building $(PLUGIN_NAME) $(VERSION) for Gotify $(GOTIFY_VERSION), linux-$(ARCH), Go $$(cat .go-version)"
	$(DOCKER_RUN) gotify/build:$$(cat .go-version)-linux-$(ARCH) \
		$(DOCKER_GO_BUILD) -o $(BUILD_DIR)/$(PLUGIN_NAME)-linux-$(ARCH)-for-gotify-$(GOTIFY_VERSION).so .
	scripts/check-plugin-abi.sh $(BUILD_DIR)/$(PLUGIN_NAME)-linux-$(ARCH)-for-gotify-$(GOTIFY_VERSION).so $(GOTIFY_VERSION)

# build-all walks the matrix and pins go.mod back to the release that was
# pinned when it started, even when a build fails. VERSION is passed down so
# that builds for another release are not stamped -dirty only because go.mod
# is re-pinned for them.
build-all:
	@pinned="$$(cat .gotify-version)"; \
	status=0; \
	for gotify in $(GOTIFY_VERSIONS); do \
		for arch in $(ARCHES); do \
			$(MAKE) --no-print-directory build-plugin GOTIFY_VERSION=$$gotify ARCH=$$arch VERSION=$(VERSION) || { status=1; break 2; }; \
		done; \
	done; \
	if [ "$$(cat .gotify-version)" != "$$pinned" ]; then \
		scripts/pin-gotify.sh "$$pinned" || status=1; \
	fi; \
	exit $$status

# The control panel only runs inside Gotify. preview serves the same page
# from a test with a sample configuration and canned answers, so a change in
# web/ can be seen in a browser without Docker. No command is really run.
preview:
	go test -tags preview -run TestPreviewPanel -timeout 0 -v .

clean:
	rm -rf $(BUILD_DIR)/ *.so

help:
	@echo ""
	@echo "  gotify-commander — Build"
	@echo "  ────────────────────────"
	@echo ""
	@echo "  make test           Run unit tests"
	@echo "  make lint           Run go vet"
	@echo "  make build          Test + lint (validation)"
	@echo "  make pin            Pin go.mod to GOTIFY_VERSION (scripts/pin-gotify.sh)"
	@echo "  make build-plugin   Build build/*.so in gotify/build and check its ABI"
	@echo "                      GOTIFY_VERSION=$(GOTIFY_VERSION) ARCH=$(ARCH) (amd64, arm64, arm-7)"
	@echo "  make build-all      Every Gotify release × arch: $(GOTIFY_VERSIONS) × $(ARCHES)"
	@echo "  make preview        Serve the control panel on 127.0.0.1:8377 with canned answers"
	@echo "  make clean          Remove build artifacts"
	@echo ""

# Deploying is left to each installation. An example that builds on the
# server and restarts Gotify; set the variables for your host first:
#
# VPS_HOST ?= your-server
# VPS_USER ?= your-user
# VPS_REPO ?= /path/to/gotify-commander
# VPS_PLUGIN_DIR ?= /path/to/gotify/data/plugins
# GOTIFY_SERVICE := gotify
#
# deploy:
# 	ssh $(VPS_USER)@$(VPS_HOST) 'cd $(VPS_REPO) && git pull && make build-plugin ARCH=amd64 \
# 		&& cp build/$(PLUGIN_NAME)-linux-amd64-for-gotify-$$(cat .gotify-version).so $(VPS_PLUGIN_DIR)/ \
# 		&& sudo systemctl restart $(GOTIFY_SERVICE)'
