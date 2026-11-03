package backup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"golang.org/x/sys/unix"
)

const archiveWireMagic = "DPLAR001"

type archiveExportHeader struct {
	Protocol int    `json:"protocol"`
	Role     string `json:"role"`
	Nonce    string `json:"nonce"`
	Receipt  []byte `json:"receipt"`
}

func (a *backupArchive) export(receipt SnapshotReceipt, nonce string, w io.Writer) error {
	if !safefs.ValidID(nonce, "n") {
		return fmt.Errorf("invalid archive request nonce")
	}
	dir, lock, err := a.openBundle(receipt)
	if err != nil {
		return err
	}
	defer dir.Close()
	defer lock.Close()
	if err = a.downloadVerificationSafe(receipt); err != nil {
		return err
	}
	file, err := dir.Open("database.db", unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	rf, err := dir.Open("receipt.json", unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(rf, protocol.MaxBackupHeader+1))
	rf.Close()
	if err != nil {
		return err
	}
	data, err := json.Marshal(archiveExportHeader{1, "archive-export", nonce, raw})
	if err != nil {
		return err
	}
	if _, err = io.WriteString(w, archiveWireMagic); err != nil {
		return err
	}
	if err = protocol.WriteBackupFrame(w, data); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(w, hash), file, receipt.Header.Size); err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if digest != receipt.Header.SHA256 {
		return fmt.Errorf("stored snapshot checksum mismatch")
	}
	footer, _ := json.Marshal(protocol.SnapshotFooter{Complete: true, HeaderSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), SHA256: digest})
	return protocol.WriteBackupFrame(w, footer)
}

func readArchiveTransferHeader(r io.Reader, nonce string) (SnapshotReceipt, []byte, []byte, error) {
	var receipt SnapshotReceipt
	magic := make([]byte, len(archiveWireMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return receipt, nil, nil, err
	}
	if string(magic) != archiveWireMagic {
		return receipt, nil, nil, fmt.Errorf("invalid archive protocol magic")
	}
	data, err := protocol.ReadBackupFrame(r)
	if err != nil {
		return receipt, nil, nil, err
	}
	var h archiveExportHeader
	if err = protocol.StrictBackupJSON(data, &h); err != nil {
		return receipt, nil, nil, err
	}
	if h.Protocol != 1 || h.Role != "archive-export" || h.Nonce != nonce {
		return receipt, nil, nil, fmt.Errorf("archive response identity mismatch")
	}
	if err = protocol.StrictBackupJSON(h.Receipt, &receipt); err != nil {
		return receipt, nil, nil, err
	}
	if err = validateReceipt(receipt, receipt.ArchiveID); err != nil {
		return receipt, nil, nil, err
	}
	return receipt, h.Receipt, data, nil
}

func readArchiveTransferBody(r io.Reader, w io.Writer, receipt SnapshotReceipt, header []byte) error {
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(w, hash), r, receipt.Header.Size); err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if digest != receipt.Header.SHA256 {
		return fmt.Errorf("archive transfer checksum mismatch")
	}
	data, err := protocol.ReadBackupFrame(r)
	if err != nil {
		return err
	}
	var footer protocol.SnapshotFooter
	if err = protocol.StrictBackupJSON(data, &footer); err != nil {
		return err
	}
	if !footer.Complete || footer.SHA256 != digest || footer.HeaderSHA256 != fmt.Sprintf("%x", sha256.Sum256(header)) {
		return fmt.Errorf("incomplete archive transfer")
	}
	var trailing [1]byte
	if n, err := r.Read(trailing[:]); n != 0 || err != io.EOF {
		return fmt.Errorf("unexpected archive stream suffix")
	}
	return nil
}

func workerAdminCommand(server config.ServerConfig, args []string) *exec.Cmd {
	command := "exec \"$HOME/.local/lib/deploy/backup-worker/deploy\" backup-worker"
	for _, arg := range args {
		command += " " + transport.ShellQuote(arg)
	}
	sshArgs := append([]string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}, transport.GetSSHBaseArgs(config.ServerEnvironment(server))...)
	return exec.Command("ssh", append(sshArgs, command)...)
}

func pullRemoteArchive(server config.ServerConfig, archive, id string, latest bool, filter ArchiveFilter, output string) (string, error) {
	nonce, err := safefs.NewID("n")
	if err != nil {
		return "", err
	}
	args := []string{"__export", "--local", "--archive", archive, "--nonce", nonce}
	if latest {
		args = append(args, "--latest")
	} else {
		args = append(args, "--backup", id)
	}
	if filter.Source != "" {
		args = append(args, "--source", filter.Source)
	}
	if filter.Instance != "" {
		args = append(args, "--instance", filter.Instance)
	}
	if filter.Generation != "" {
		args = append(args, "--generation", filter.Generation)
	}
	cmd := workerAdminCommand(server, args)
	stdout, pipeWriter, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer stdout.Close()
	var stderr process.Buffer
	stderr.Limit = 4096
	cmd.Stderr = &stderr
	cmd.Stdout = pipeWriter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		defer pipeWriter.Close()
		runErr = process.RunContext(ctx, cmd, 20*time.Minute, 10*time.Second)
	}()
	defer func() { cancel(); <-done }()
	receipt, raw, header, err := readArchiveTransferHeader(stdout, nonce)
	if err != nil {
		return "", err
	}
	if id != "" && receipt.BackupID != id {
		return "", fmt.Errorf("worker exported a different backup")
	}
	if safefs.ValidID(filter.Source, "s") && receipt.Header.Attempt.SourceID != filter.Source {
		return "", fmt.Errorf("worker exported a different source")
	}
	if filter.Instance != "" && receipt.Header.Attempt.Service != filter.Instance {
		return "", fmt.Errorf("worker exported a different instance")
	}
	if filter.Generation != "" && receipt.Header.Generation != filter.Generation {
		return "", fmt.Errorf("worker exported a different generation")
	}
	return publishLocalPull(receipt, raw, output, func(file *os.File) error {
		if err := readArchiveTransferBody(stdout, file, receipt, header); err != nil {
			return err
		}
		<-done
		return runErr
	}, func(file *os.File) (protocol.SnapshotValidation, error) {
		return isolatedSnapshotValidation(file, false, 5*time.Minute)
	})
}
