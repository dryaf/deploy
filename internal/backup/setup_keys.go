package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
)

// A declarative source key is pinned before the prepared response can expose it
// for enrollment. Losing that key can never authorize an automatic replacement.
func setupSourceKey(home string, keys, stateRoot *safefs.Directory, j *WorkerSetupJournal, alias string) (string, error) {
	if !config.ValidInventoryName(alias) {
		return "", fmt.Errorf("invalid source alias")
	}
	pin := j.SourceKeys[alias]
	if enrolled, ok := j.SourceEnrollments[alias]; ok {
		if pin != "" && pin != enrolled.Fingerprint {
			return "", fmt.Errorf("source key reservation differs from enrollment")
		}
		pin = enrolled.Fingerprint
	}
	file := alias + ".key"
	f, err := keys.Open(file, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		if j.Declaration != nil && pin != "" {
			return "", fmt.Errorf("reserved source key is missing; explicit key recovery required")
		}
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "deploy-backup:"+j.WorkerID+":"+alias, "-f", filepath.Join(keys.File().Name(), file))
		if _, err = process.Output(cmd, 10*time.Second); err != nil {
			return "", fmt.Errorf("source key creation failed")
		}
		f, err = keys.Open(file, os.O_RDONLY, 0)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", fmt.Errorf("unsafe worker source key")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("unsafe source key owner or hardlink")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = keys.File().Sync(); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 {
		return "", fmt.Errorf("invalid worker source key")
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return "", fmt.Errorf("worker source identity is not Ed25519")
	}
	fingerprint := ssh.FingerprintSHA256(signer.PublicKey())
	if j.Declaration != nil {
		if pin != "" && pin != fingerprint {
			return "", fmt.Errorf("reserved source key changed; explicit key recovery required")
		}
		if j.SourceKeys == nil {
			j.SourceKeys = map[string]string{}
		}
		if j.SourceKeys[alias] == "" {
			j.SourceKeys[alias] = fingerprint
			if err = stateRoot.WriteJSON("setup.json", j, true); err != nil {
				return "", err
			}
		}
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey())), nil
}

// Completed retries and status must verify keys too. The absence of a key is
// recovery work, not a reason to report an unchanged usable installation.
func verifySetupSourceKeys(root *safefs.Directory, j WorkerSetupJournal) error {
	if j.Declaration == nil {
		return nil
	}
	if j.SourceRetirement != nil {
		return checkSetupSourceRetirement(root, j, nil)
	}
	if len(j.SourceKeys) != len(j.Declaration.Sources) {
		return fmt.Errorf("backup source key reservations are incomplete")
	}
	paths, err := declarationPaths(root.File().Name(), j.Declaration)
	if err != nil {
		return err
	}
	for alias := range j.Declaration.Sources {
		pin := j.SourceKeys[alias]
		if !config.ValidInventoryName(alias) || pin == "" || j.SourceEnrollments[alias].Fingerprint != pin {
			return fmt.Errorf("backup source enrollment differs from key reservation")
		}
		f, err := openBackupOwnedFile(root, paths.OwnedPath(filepath.Join(paths.Keys(), alias+".key")), os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("reserved backup source key unavailable")
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
			f.Close()
			return fmt.Errorf("unsafe reserved backup source key")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
			f.Close()
			return fmt.Errorf("unsafe reserved backup source key ownership")
		}
		raw, e := io.ReadAll(io.LimitReader(f, 16385))
		f.Close()
		if e != nil || len(raw) > 16384 {
			return fmt.Errorf("reserved backup source key unreadable")
		}
		key, e := ssh.ParsePrivateKey(raw)
		if e != nil || key.PublicKey().Type() != ssh.KeyAlgoED25519 || ssh.FingerprintSHA256(key.PublicKey()) != pin {
			return fmt.Errorf("reserved backup source key changed")
		}
	}
	return nil
}
