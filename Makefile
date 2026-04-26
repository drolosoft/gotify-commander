PLUGIN_NAME := gotify-commander
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

VPS_HOST ?= your-server
VPS_USER ?= your-user
VPS_REPO ?= /path/to/gotify-commander
VPS_PLUGIN_DIR ?= /path/to/gotify/data/plugins
GOTIFY_SERVICE := gotify
GOTIFY_BUILD_IMAGE := gotify/build:1.23.3-linux-amd64
DOCKER_GO_BUILD := go build -mod=readonly -a -installsuffix cgo -buildmode=plugin

.PHONY: test lint build deploy deploy-dev clean help

test:
	go test ./... -v -count=1

lint:
	go vet ./...

build: test lint
	@echo "✅ Tests and lint passed (plugin .so must be built via Docker on Linux)"

deploy:
	@echo "🚀 Deploying to VPS..."
	ssh $(VPS_USER)@$(VPS_HOST) 'cd $(VPS_REPO) && git pull && sudo docker run --rm -v "$$PWD:/proj" -w /proj $(GOTIFY_BUILD_IMAGE) $(DOCKER_GO_BUILD) -o $(PLUGIN_NAME).so . && cp $(PLUGIN_NAME).so $(VPS_PLUGIN_DIR)/ && sudo systemctl restart $(GOTIFY_SERVICE)'
	@echo "✅ Deployed and Gotify restarted"

deploy-dev:
	@echo "🚀 Fast deploy (skip tests)..."
	ssh $(VPS_USER)@$(VPS_HOST) 'cd $(VPS_REPO) && git pull && sudo docker run --rm -v "$$PWD:/proj" -w /proj $(GOTIFY_BUILD_IMAGE) $(DOCKER_GO_BUILD) -o $(PLUGIN_NAME).so . && cp $(PLUGIN_NAME).so $(VPS_PLUGIN_DIR)/ && sudo systemctl restart $(GOTIFY_SERVICE)'
	@echo "✅ Deployed"

clean:
	rm -rf build/ *.so

help:
	@echo ""
	@echo "  gotify-commander — Build & Deploy"
	@echo "  ──────────────────────────────────"
	@echo ""
	@echo "  make test         Run unit tests"
	@echo "  make lint         Run go vet"
	@echo "  make build        Test + lint (validation)"
	@echo "  make deploy       SSH → build on VPS → install → restart Gotify"
	@echo "  make deploy-dev   Fast deploy, skip tests"
	@echo "  make clean        Remove build artifacts"
	@echo ""
