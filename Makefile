# Installs the deploy tool to ~/bin

BINARY_NAME=deploy
INSTALL_DIR=$(HOME)/bin

.PHONY: all build install clean

all: build

build:
	# Builds all files in the current directory (package main)
	go build -o $(BINARY_NAME) .

install: build
	mkdir -p $(INSTALL_DIR)
	mv $(BINARY_NAME) $(INSTALL_DIR)/$(BINARY_NAME)
	@echo "✅ Installed 'deploy' to $(INSTALL_DIR)"
	@echo "   Ensure $(INSTALL_DIR) is in your PATH."

clean:
	rm -f $(BINARY_NAME)
.PHONY: test test-image-deploy-integration

test:
	go test ./...
	go test -race ./...
	go vet ./...

# Run only inside a disposable rootless Podman/systemd Linux VM or test account.
# Explicit environment opt-in and a verified Gotenberg digest are required.
test-image-deploy-integration:
	@test "$(DEPLOY_IMAGE_INTEGRATION)" = isolated || (echo 'Set DEPLOY_IMAGE_INTEGRATION=isolated in a disposable test environment'; exit 1)
	@test -n "$(DEPLOY_TEST_GOTENBERG_IMAGE)" || (echo 'Set DEPLOY_TEST_GOTENBERG_IMAGE to a verified Chromium image with digest'; exit 1)
	go test -tags=imageintegration -run '^TestRootlessImageDeploymentIntegration$$' -v -timeout=10m .

.PHONY: test-image-deploy-isolated

# Creates a separate rootless Podman store and a disposable systemd container.
test-image-deploy-isolated:
	./scripts/test-image-deploy-isolated.sh

.PHONY: test-deploy-vm-crash release-check

test-deploy-vm-crash:
	./scripts/test-deploy-vm-crash.sh

# Required before publishing a deploy binary. The CI release-gate has the same dependencies.
release-check: test test-image-deploy-isolated test-deploy-vm-crash
