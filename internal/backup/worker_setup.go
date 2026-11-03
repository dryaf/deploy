package backup

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"github.com/dryaf/deploy/internal/ui"
)

func remoteHelperArtifact(server config.ServerConfig, artifacts string) (string, error) {
	output, err := process.LimitedOutput(transport.WorkerAdminShell(server, "uname -s; uname -m"), 20*time.Second, 4096)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[0] != "Linux" {
		return "", fmt.Errorf("backup helpers require Linux")
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[fields[1]]
	if arch == "" {
		return "", fmt.Errorf("source/worker requires 64-bit AMD64 or ARM64 Linux")
	}
	file := filepath.Join(artifacts, "deploy-linux-"+arch)
	binary, err := elf.Open(file)
	if err != nil {
		return "", fmt.Errorf("build helper artifacts first: %w", err)
	}
	defer binary.Close()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if binary.Machine != machine {
		return "", fmt.Errorf("helper artifact architecture mismatch")
	}
	for _, p := range binary.Progs {
		if p.Type == elf.PT_INTERP {
			return "", fmt.Errorf("backup helper artifact must be statically linked (CGO_ENABLED=0)")
		}
	}
	return file, nil
}

func bootstrapHelperCommand(server config.ServerConfig, artifact, operation string) (*exec.Cmd, []byte, error) {
	if operation != "__worker_setup" && operation != "__source_admin" {
		return nil, nil, fmt.Errorf("invalid bootstrap operation")
	}
	binary, err := os.ReadFile(artifact)
	if err != nil {
		return nil, nil, err
	}
	if len(binary) > 128<<20 {
		return nil, nil, fmt.Errorf("helper artifact exceeds bootstrap bound")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	script := fmt.Sprintf(`import os,sys,tempfile,hashlib,subprocess
size=%d
expected=%q
root=tempfile.mkdtemp(prefix="deploy-backup-bootstrap-")
path=os.path.join(root,"deploy")
try:
 h=hashlib.sha256()
 with open(path,"xb") as out:
  os.chmod(path,0o700)
  while size:
   data=os.read(0,min(size,65536))
   if not data: raise RuntimeError("truncated helper artifact")
   out.write(data);h.update(data);size-=len(data)
  out.flush();os.fsync(out.fileno())
 if h.hexdigest()!=expected: raise RuntimeError("helper digest mismatch")
 status=subprocess.call([path,%q])
finally:
 os.unlink(path);os.rmdir(root)
sys.exit(status)
`, len(binary), digest, operation)
	return transport.WorkerAdminShell(server, "python3 -I -c "+transport.ShellQuote(script)), binary, nil
}

func verifiedKnownHosts(server config.ServerConfig) (string, error) {
	file := server.KnownHostsFile
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		file = filepath.Join(home, ".ssh/known_hosts")
	}
	host := server.Host
	if server.SSHPort != 0 && server.SSHPort != 22 {
		host = fmt.Sprintf("[%s]:%d", host, server.SSHPort)
	}
	body, err := process.LimitedOutput(exec.Command("ssh-keygen", "-F", host, "-f", file), 5*time.Second, 16384)
	if err != nil {
		return "", fmt.Errorf("source has no matching operator-verified known-host entry")
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("source host key must already be verified in the operator known-hosts file")
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func setupBackupWorker(args []string) error { return setupBackupWorkerOperation("setup", args) }

func setupBackupWorkerOperation(operation string, args []string) error {
	flags := flag.NewFlagSet("backup-worker setup", flag.ContinueOnError)
	recoverState := flags.Bool("recover", false, "Adopt the explicitly reconstructed worker state/archive")
	fenced := flags.Bool("fenced", false, "Acknowledge the former writer has been disabled")
	image := flags.String("image", "", "Explicit worker image pinned by digest")
	relay := flags.String("relay-image", "", "Explicit Signal relay image pinned by digest")
	artifacts := flags.String("artifacts", "dist/backup-worker", "Directory containing matching static Linux helper artifacts")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("setup requires a named worker")
	}
	inventory, err := config.ReadInventory(ui.InventoryPath(""))
	if err != nil {
		return err
	}
	worker, ok := inventory.Servers[flags.Arg(0)]
	if !ok || worker.BackupWorker == nil {
		return fmt.Errorf("selected server has no backup_worker policy")
	}
	if err = config.PinnedWorkerImage(*image); err != nil {
		return err
	}
	if worker.BackupWorker.Notifications.Signal != nil {
		if err = config.PinnedWorkerImage(*relay); err != nil {
			return fmt.Errorf("Signal requires --relay-image pinned by digest")
		}
	}
	request := WorkerSetupRequest{Recover: *recoverState, Fenced: *fenced, Operation: operation, Schema: 1, Policy: *worker.BackupWorker, Image: *image, RelayImage: *relay}
	if ui.DryRun {
		fmt.Printf("[DRY] Stage worker %s (%s@%s), archive %s on required mount %s\n", flags.Arg(0), worker.User, worker.Host, request.Policy.Storage.Path, request.Policy.Storage.RequireMount)
		fmt.Printf("[DRY] Sources: %s; schedule %s; provider %s; worker image %s\n", strings.Join(request.Policy.Sources, ", "), request.Policy.Schedule, request.Policy.Notifications.Primary, *image)
		fmt.Println("[DRY] No SSH, key creation, image pulls or writes. Remote identities and qualification remain unknown; timers require explicit qualification/activation.")
		return nil
	}
	for _, alias := range request.Policy.Sources {
		server := inventory.Servers[alias]
		known, err := verifiedKnownHosts(server)
		if err != nil {
			return fmt.Errorf("source %s: %w", alias, err)
		}
		port := server.SSHPort
		if port == 0 {
			port = 22
		}
		request.Sources = append(request.Sources, SetupSource{alias, server.Host, server.User, port, known})
	}
	artifact, err := remoteHelperArtifact(worker, *artifacts)
	if err != nil {
		return err
	}
	cmd, binary, err := bootstrapHelperCommand(worker, artifact, "__worker_setup")
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = stdoutWriter
	var stderr process.Buffer
	stderr.Limit = 8192
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	go func() { defer stdoutWriter.Close(); done <- process.Run(cmd, 60*time.Minute, 10*time.Second) }()
	finished := false
	defer func() {
		stdin.Close()
		stdout.Close()
		if !finished {
			<-done
		}
	}()
	if _, err = stdin.Write(binary); err != nil {
		return err
	}
	body, _ := json.Marshal(request)
	if err = protocol.WriteBackupFrame(stdin, body); err != nil {
		return err
	}
	readResponse := func() (WorkerSetupResponse, error) {
		var response WorkerSetupResponse
		raw, err := protocol.ReadBackupFrame(stdout)
		if err != nil {
			return response, fmt.Errorf("setup coordinator failed before response")
		}
		if err = protocol.StrictBackupJSON(raw, &response); err != nil {
			return response, err
		}
		if response.Error != "" {
			return response, fmt.Errorf("worker setup: %s", response.Error)
		}
		return response, nil
	}
	response, err := readResponse()
	if err != nil {
		return err
	}
	if response.Stage == "complete" {
		stdin.Close()
		err = <-done
		finished = true
		if err != nil {
			return err
		}
		fmt.Println("Worker already staged; installation identities and owned files match.")
		return nil
	}
	if response.Stage != "prepared" || !safefs.ValidID(response.WorkerID, "w") {
		return fmt.Errorf("invalid setup coordinator response")
	}
	for _, source := range request.Sources {
		server := inventory.Servers[source.Alias]
		artifact, err := remoteHelperArtifact(server, *artifacts)
		if err != nil {
			return err
		}
		sourceCommand, binary, err := bootstrapHelperCommand(server, artifact, "__source_admin")
		if err != nil {
			return err
		}
		enrollment := deploysource.SourceEnrollmentRequest{Schema: 1, Operation: "enroll", WorkerID: response.WorkerID, PublicKey: response.PublicKeys[source.Alias]}
		body, _ := json.Marshal(enrollment)
		sourceCommand.Stdin = io.MultiReader(bytes.NewReader(binary), bytes.NewReader(body))
		output, err := process.LimitedOutput(sourceCommand, 2*time.Minute, protocol.MaxBackupHeader)
		if err != nil {
			return fmt.Errorf("source enrollment failed for %s; setup journal retained for retry", source.Alias)
		}
		var enrolled deploysource.SourceEnrollmentResult
		if err = protocol.StrictBackupJSON(output, &enrolled); err != nil {
			return err
		}
		step, _ := json.Marshal(WorkerSetupStep{Operation: "source", Alias: source.Alias, Enrollment: &enrolled})
		if err = protocol.WriteBackupFrame(stdin, step); err != nil {
			return err
		}
		if _, err = readResponse(); err != nil {
			return err
		}
	}
	finish, _ := json.Marshal(WorkerSetupStep{Operation: "finish"})
	if err = protocol.WriteBackupFrame(stdin, finish); err != nil {
		return err
	}
	if response, err = readResponse(); err != nil {
		return err
	}
	stdin.Close()
	err = <-done
	finished = true
	if err != nil {
		return fmt.Errorf("setup coordinator did not exit successfully")
	}
	if response.Stage != "complete" {
		return fmt.Errorf("setup did not commit")
	}
	fmt.Println("Worker staged. Timers remain disabled pending live qualification and explicit activation. Manual run, status, list and pull are available.")
	return nil
}
