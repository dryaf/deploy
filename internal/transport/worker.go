package transport

import (
	"os/exec"

	"github.com/dryaf/deploy/internal/config"
)

func WorkerAdminShell(server config.ServerConfig, command string) *exec.Cmd {
	env := config.ServerEnvironment(server)
	args := append([]string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}, GetSSHBaseArgs(env)...)
	args = append(args, command)
	return exec.Command("ssh", args...)
}
