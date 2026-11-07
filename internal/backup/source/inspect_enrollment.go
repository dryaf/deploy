package source

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// inspectSourceEnrollment is deliberately read-only. In particular an endpoint
// move must not install a helper, create source state or grant a key on the wrong
// machine before discovering that it is a different source.
func inspectSourceEnrollment(home string, r SourceEnrollmentRequest) (SourceEnrollmentResult, error) {
	var result SourceEnrollmentResult
	if r.Schema != 1 || r.Operation != "inspect" || !safefs.ValidID(r.WorkerID, "w") || !safefs.ValidID(r.SourceID, "s") || !safefs.DigestPattern.MatchString(r.HelperSHA256) || r.Previous != "" || r.Service != "" || r.Reason != "" {
		return result, fmt.Errorf("source inspection requires exact retained identity and helper pins")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(r.PublicKey))
	if err != nil || key.Type() != ssh.KeyAlgoED25519 || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return result, fmt.Errorf("source inspection requires one retained Ed25519 public key")
	}
	d, err := safefs.Open(filepath.Join(home, ".local/state/deploy/backup-source"))
	if err != nil {
		return result, err
	}
	defer d.Close()
	lock, err := d.Lock("policy.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var identity SourceIdentity
	if err = d.ReadJSON("identity.json", &identity); err != nil {
		return result, err
	}
	known := identity.Workers[r.WorkerID]
	if identity.Schema != 1 || identity.ID != r.SourceID || !known.Enabled || known.Fingerprint != ssh.FingerprintSHA256(key) {
		return result, fmt.Errorf("endpoint does not retain the selected source and worker authorization")
	}
	if err = verifyRetainedSourceHelper(home, r.HelperSHA256); err != nil {
		return result, err
	}
	return SourceEnrollmentResult{Schema: 1, SourceID: identity.ID, WorkerID: r.WorkerID, Fingerprint: known.Fingerprint}, nil
}

// Called under the existing source policy lease; never installs or repairs files.
func verifyRetainedSourceHelper(home, digest string) error {
	artifacts, err := safefs.Open(filepath.Join(home, ".local/lib/deploy/backup-source"))
	if err != nil {
		return err
	}
	defer artifacts.Close()
	var manifest helper.HelperArtifactManifest
	if err = artifacts.ReadJSON("artifact.json", &manifest); err != nil {
		return err
	}
	if manifest.Schema != 1 || manifest.Role != "backup-source" || manifest.Pending != "" || manifest.Current != digest {
		return fmt.Errorf("retained source helper differs; apply artifact updates separately")
	}
	f, err := artifacts.Open("deploy", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 128<<20 || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe retained source helper")
	}
	hash, err := safefs.DigestFile(f)
	if err != nil || hash != digest {
		return fmt.Errorf("retained source helper changed")
	}
	return nil
}
