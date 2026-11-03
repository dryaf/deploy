package signalaccount

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// The dedicated offline script runs this in the digest-pinned image with no
// network and tmpfs-only state. It must not run against an operator account.
func TestSignalLinkOfflineCLI(t *testing.T) {
	if os.Getenv("DEPLOY_TEST_OFFLINE_SIGNAL_LINK") != "1" {
		t.Skip("offline pinned-container fixture only")
	}
	t.Setenv("PATH", "/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin")
	version, err := process.LimitedOutput(exec.Command("signal-cli", "--version"), 10*time.Second, 4096)
	if err != nil || strings.TrimSpace(string(version)) != "signal-cli 0.14.9" {
		t.Fatal("pinned CLI version", err, string(version))
	}
	path, id, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := Link(ctx, path, "offline-disposable", id, io.Discard)
	if err == nil || status.State != "uncertain" || status.RuntimeAllowed {
		t.Fatal("offline linking did not preserve uncertainty", status, err)
	}
	requireRuntime(t, path, false)
	status, err = Inspect(path)
	if err != nil || status.State != "uncertain" {
		t.Fatal("CLI children retained lease after bounded shutdown", status, err)
	}
	status, err = Resolve(path, id, "Reviewed disposable network-isolated account after expected CLI failure")
	if err != nil || !status.RuntimeAllowed || status.State != "reviewed" {
		t.Fatal(status, err)
	}
}
