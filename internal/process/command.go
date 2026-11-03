package process

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/ui"
)

func GetCmdOutput(name string, args ...string) string {
	out, _ := Output(exec.Command(name, args...), 2*time.Minute)
	return strings.TrimSpace(string(out))
}

func RunCommand(desc string, cmd *exec.Cmd) error {
	return RunCommandBudget(desc, cmd, 15*time.Minute, 10*time.Second)
}

func RunCommandBudget(desc string, cmd *exec.Cmd, duration, grace time.Duration) error {
	if ui.DryRun {
		ui.LogDebug("[DRY] %s", cmd.String())
		return nil
	}
	if ui.Verbose {
		ui.LogDebug("[EXEC] %s", cmd.String())
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := Run(cmd, duration, grace); err != nil {
			return fmt.Errorf("%s: %w\nSTDOUT:\n%s\nSTDERR:\n%s", desc, err, outBuf.String(), errBuf.String())
		}
		return nil
	}
	return Run(cmd, duration, grace)
}

func RunCommandRaw(name string, args ...string) error {
	if ui.DryRun {
		fmt.Printf("[DRY] %s %v\n", name, args)
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return Run(cmd, 2*time.Minute, 10*time.Second)
}
