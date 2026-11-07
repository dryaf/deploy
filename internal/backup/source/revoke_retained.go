package source

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// revokeRetainedSource retires an exact authorization without creating source
// state or installing a helper. The disabled authorization retains its identity
// so a lost acknowledgement or a crash before key removal can be retried.
func revokeRetainedSource(home string, r SourceEnrollmentRequest) (SourceEnrollmentResult, error) {
	var result SourceEnrollmentResult
	if r.Schema != 1 || r.Operation != "revoke-retained" || !safefs.ValidID(r.WorkerID, "w") || !safefs.ValidID(r.SourceID, "s") || !safefs.DigestPattern.MatchString(r.HelperSHA256) || r.Previous == "" || r.PublicKey != "" || r.Service != "" || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1024 || strings.ContainsAny(r.Reason, "\x00\r\n") {
		return result, fmt.Errorf("source retirement requires exact retained identity, key, helper and disposition")
	}
	d, err := safefs.Open(filepath.Join(home, ".local/state/deploy/backup-source"))
	if err != nil {
		return result, err
	}
	defer d.Close()
	lock, err := d.Lock("policy.lock", true, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var identity SourceIdentity
	if err = d.ReadJSON("identity.json", &identity); err != nil {
		return result, err
	}
	known, ok := identity.Workers[r.WorkerID]
	if identity.Schema != 1 || identity.ID != r.SourceID || !ok || known.Fingerprint != r.Previous || known.RetirementReason != "" && known.RetirementReason != r.Reason {
		return result, fmt.Errorf("source retirement does not match retained authorization")
	}
	if err = verifyRetainedSourceHelper(home, r.HelperSHA256); err != nil {
		return result, err
	}
	sshDir, err := safefs.Open(filepath.Join(home, ".ssh"))
	if err != nil {
		return result, err
	}
	defer sshDir.Close()
	info, err := sshDir.File().Stat()
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return result, fmt.Errorf("source .ssh must be private")
	}
	f, err := sshDir.Open("authorized_keys", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		f.Close()
		return result, fmt.Errorf("unsafe source authorized_keys")
	}
	old, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	f.Close()
	if err != nil || len(old) > 1<<20 {
		return result, fmt.Errorf("cannot read bounded source authorized_keys")
	}
	marker := "deploy-backup:" + r.WorkerID
	var next []byte
	found := false
	// Preserve all unrelated bytes, including comments and blank lines.
	for _, line := range bytes.SplitAfter(old, []byte("\n")) {
		if !bytes.Contains(line, []byte(marker)) {
			next = append(next, line...)
			continue
		}
		key, comment, options, rest, e := ssh.ParseAuthorizedKey(line)
		if e != nil || comment != marker || len(bytes.TrimSpace(rest)) != 0 {
			return result, fmt.Errorf("conflicting source authorized_keys entry")
		}
		fingerprint := ssh.FingerprintSHA256(key)
		forced := "command=\"" + filepath.Join(home, ".local/lib/deploy/backup-source/deploy") + " source-serve --worker-id " + r.WorkerID + " --key-id " + fingerprint + "\""
		hasForced, hasRestrict := false, false
		for _, option := range options {
			hasForced = hasForced || option == forced
			hasRestrict = hasRestrict || option == "restrict"
		}
		if !hasForced || !hasRestrict || fingerprint != known.Fingerprint && fingerprint != known.Previous {
			return result, fmt.Errorf("source key ownership changed before retirement")
		}
		found = found || fingerprint == known.Fingerprint
	}
	if known.Enabled && !found {
		return result, fmt.Errorf("enabled source authorization has lost its retained key")
	}
	// Validate all authority before the first write. Disable policy before
	// removing SSH access: interruption can never leave a usable retired key.
	if known.Enabled || known.RetirementReason == "" {
		known.Enabled = false
		known.RetirementReason = r.Reason
		identity.Workers[r.WorkerID] = known
		if err = d.WriteJSON("identity.json", identity, true); err != nil {
			return result, err
		}
	}
	if !bytes.Equal(old, next) {
		if err = safefs.ReplaceFile(sshDir, "authorized_keys", old, next, 0600); err != nil {
			return result, err
		}
	}
	return SourceEnrollmentResult{Schema: 1, SourceID: identity.ID, WorkerID: r.WorkerID}, nil
}
