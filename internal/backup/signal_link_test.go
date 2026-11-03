package backup

import (
	"strings"
	"testing"
)

func TestSignalLinkUsesFencedContainerEntryPoint(t *testing.T) {
	cmd := signalLinkCommand("/private/account", "example/image@sha256:"+strings.Repeat("a", 64), "n-12345678901234567890123456789012")
	args := strings.Join(cmd.Args, " ")
	for _, expected := range []string{"--pull=never", "--read-only", "--cap-drop=all", "--volume /private/account:/signal-state:rw", "--entrypoint=/usr/local/bin/deploy", "notification-worker __signal-link --local --state-dir /signal-state --operation", "--name deploy-backup"} {
		if !strings.Contains(args, expected) {
			t.Fatal("missing linking guard", expected, args)
		}
	}
	if strings.Contains(args, "--entrypoint=signal-cli") {
		t.Fatal("link bypasses account lease")
	}
}
