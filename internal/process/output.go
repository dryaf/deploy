package process

import (
	"fmt"
	"os/exec"
	"time"
)

func LimitedOutput(cmd *exec.Cmd, timeout time.Duration, limit int) ([]byte, error) {
	var out, stderr Buffer
	out.Limit = limit
	stderr.Limit = 4096
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := Run(cmd, timeout, 10*time.Second); err != nil {
		return nil, fmt.Errorf("bounded prerequisite command failed: %w", err)
	}
	return out.Bytes(), nil
}
