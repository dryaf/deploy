package signalaccount

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

// ResolveNativeAccountIdentity selects exactly one public ACI from bounded
// listAccounts output. Private account selectors are never returned or logged.
func ResolveNativeAccountIdentity(raw []byte, selector string) (string, error) {
	var accounts []struct {
		Number string `json:"number"`
		ACI    string `json:"aci"`
	}
	if err := safefs.ReadJSON(bytes.NewReader(raw), &accounts, 32<<10); err != nil {
		return "", fmt.Errorf("invalid Signal account identity response")
	}
	matches := 0
	identity := ""
	for _, account := range accounts {
		if account.Number == selector || account.ACI == selector {
			matches++
			identity = account.ACI
		}
	}
	if matches != 1 || !provider.ValidSignalAccount(identity) {
		return "", fmt.Errorf("Signal account identity unavailable or ambiguous")
	}
	return identity, nil
}

// InspectNativeAccount probes the actual linked account under its exclusive
// lease. It performs no send/link and requires the retained native link pins.
func InspectNativeAccount(ctx context.Context, path, selectorFile string, native NativeCLI, operation, recordHash string) (string, error) {
	if !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(recordHash) {
		return "", fmt.Errorf("native link evidence required")
	}
	selector, err := provider.ReadSecret(selectorFile)
	if err != nil {
		return "", fmt.Errorf("private Signal account selector unavailable")
	}
	cli, err := native.open()
	if err != nil {
		return "", err
	}
	defer cli.Close()
	lease, err := AcquireRuntime(path)
	if err != nil {
		return "", err
	}
	defer lease.Close()
	h, err := readHistory(lease.dir)
	if err != nil {
		return "", err
	}
	if len(h.Records) == 0 || h.Pending {
		return "", fmt.Errorf("completed native linking required")
	}
	last := h.Records[len(h.Records)-1]
	if last.Intent.ID != operation || protocol.Hash(last) != recordHash || last.Intent.Native == nil || last.Intent.Native.SHA256 != native.SHA256 || last.Intent.Native.Version != native.Version {
		return "", fmt.Errorf("native Signal link evidence changed")
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("/proc/self/fd/4", args...)
		cmd.ExtraFiles = []*os.File{lease.File(), cli}
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + path, "LANG=C.UTF-8"}
		var output process.Buffer
		output.Limit = 32 << 10
		cmd.Stdout = &output
		if err := process.RunContext(ctx, cmd, 15*time.Second, time.Second); err != nil {
			return nil, fmt.Errorf("native Signal account probe failed")
		}
		return output.Bytes(), nil
	}
	version, err := run("--version")
	if err != nil || strings.TrimSpace(string(version)) != "signal-cli "+native.Version {
		return "", fmt.Errorf("native Signal CLI version probe failed")
	}
	raw, err := run("--config", path, "--output=json", "listAccounts")
	if err != nil {
		return "", err
	}
	return ResolveNativeAccountIdentity(raw, selector)
}
