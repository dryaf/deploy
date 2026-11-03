# Installs the deploy tool to ~/bin

BINARY_NAME=deploy
INSTALL_DIR=$(HOME)/bin

.PHONY: all build install clean

all: build

build:
	# Canonical CLI entry point; the root main.go remains an install-compatible shim.
	go build -o $(BINARY_NAME) ./cmd/deploy

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

.PHONY: test-backup-foundations

# Local prerequisites only: this is not the ARM64/Signal/worker acceptance gate.
# These cases also run in the complete test and release-check targets.
test-backup-foundations:
	go test -race -run '^Test(ServerReference|ServerInventory|Inventory|InlineAndMixed|BackupWorkerPolicy|BackupBinding|QuadletMetadata|SQLite|BoundedOutput|InstalledQuadletGenerator)' ./...

# Run only inside a disposable rootless Podman/systemd Linux VM or test account.
# Explicit environment opt-in and a verified Gotenberg digest are required.
test-image-deploy-integration:
	@test "$(DEPLOY_IMAGE_INTEGRATION)" = isolated || (echo 'Set DEPLOY_IMAGE_INTEGRATION=isolated in a disposable test environment'; exit 1)
	@test -n "$(DEPLOY_TEST_GOTENBERG_IMAGE)" || (echo 'Set DEPLOY_TEST_GOTENBERG_IMAGE to a verified Chromium image with digest'; exit 1)
	go test -tags=imageintegration -run '^TestRootlessImageDeploymentIntegration$$' -v -timeout=10m ./internal/deployment

.PHONY: test-image-deploy-isolated

# Creates a separate rootless Podman store and a disposable systemd container.
test-image-deploy-isolated:
	./scripts/test-image-deploy-isolated.sh

.PHONY: test-deploy-vm-crash release-check

test-deploy-vm-crash:
	./scripts/test-deploy-vm-crash.sh

# Required before publishing a deploy binary.
release-check: test test-image-deploy-isolated test-backup-worker-isolated test-notification-worker-isolated test-deploy-vm-crash

.PHONY: backup-artifacts backup-images test-backup-worker
backup-artifacts:
	./scripts/build-backup-artifacts.sh
backup-images: backup-artifacts
	./scripts/build-backup-images.sh
test-backup-worker:
	go test -race -run '^Test(Backup|ServerReference|ServerInventory|Inventory|InlineAndMixed|QuadletMetadata|SQLite|BoundedOutput|InstalledQuadletGenerator)' ./...

.PHONY: test-backup-worker-isolated
test-backup-worker-isolated:
	DEPLOY_TEST_BACKUP_WORKER=1 ./scripts/test-image-deploy-isolated.sh

.PHONY: test-notification-worker-isolated
test-notification-worker-isolated:
	./scripts/test-notification-worker-isolated.sh

.PHONY: test-signal-cli-offline
test-signal-cli-offline:
	./scripts/test-signal-cli-offline.sh
