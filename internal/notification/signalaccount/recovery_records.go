package signalaccount

import (
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const RecoveryVersion = 1
const recoveryIntentFile = "deploy-recovery-intent.json"
const recoveryFenceFile = "deploy-recovery-fence.json"
const recoveryResultFile = "deploy-recovery-result.json"
const recoveryCompleteFile = "deploy-recovery-complete.json"

// Target binds the caller's exact gateway publication, inbox transition and
// configuration. It is evidence, never permission to start an account consumer.
type RecoveryRequest struct {
	Operation string `json:"operation"`
	Binding   string `json:"binding"`
	Target    string `json:"target"`
}

func (r RecoveryRequest) validate() error {
	if !safefs.ValidID(r.Operation, "n") || !safefs.DigestPattern.MatchString(r.Binding) || !safefs.DigestPattern.MatchString(r.Target) {
		return fmt.Errorf("Signal recovery requires operation, service binding and target pins")
	}
	return nil
}

type recoveryIntent struct {
	Ancestor                                                 string `json:"ancestor,omitempty"`
	Version                                                  int
	Request                                                  RecoveryRequest
	DirectoryDevice, DirectoryInode, LeaseDevice, LeaseInode uint64
	LinkHash, MaintenanceHash                                string
}
type recoveryResult struct{ Intent, Result string }
type recoveryRecords struct {
	Intent                                   recoveryIntent
	Result                                   recoveryResult
	HasIntent, HasFence, HasResult, Complete bool
}

type RecoveryStatus struct {
	Operation      string `json:"operation,omitempty"`
	Binding        string `json:"binding,omitempty"`
	Target         string `json:"target,omitempty"`
	Phase          string `json:"phase"`
	ResultHash     string `json:"result_hash,omitempty"`
	RuntimeAllowed bool   `json:"runtime_allowed"`
}

func (r recoveryRecords) status() RecoveryStatus {
	s := RecoveryStatus{Phase: "none", RuntimeAllowed: !r.HasIntent}
	if !r.HasIntent {
		return s
	}
	s.Operation, s.Binding, s.Target, s.Phase = r.Intent.Request.Operation, r.Intent.Request.Binding, r.Intent.Request.Target, "prepared"
	if r.HasFence {
		s.Phase = "fenced"
	}
	if r.HasResult {
		s.Phase = "result"
		s.ResultHash = r.Result.Result
	}
	if r.Complete {
		s.Phase = "complete"
	}
	return s
}
func fileIdentity(info os.FileInfo) (uint64, uint64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("Signal file identity unavailable")
	}
	return uint64(st.Dev), st.Ino, nil
}
func recoveryIdentity(d *safefs.Directory) (v recoveryIntent, err error) {
	info, err := d.File().Stat()
	if err != nil {
		return v, err
	}
	v.DirectoryDevice, v.DirectoryInode, err = fileIdentity(info)
	if err != nil {
		return v, err
	}
	f, err := d.Open("deploy-account.lock", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return v, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return v, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return v, fmt.Errorf("unsafe Signal account lease")
	}
	v.LeaseDevice, v.LeaseInode, err = fileIdentity(info)
	return
}
func readRecovery(d *safefs.Directory) (r recoveryRecords, err error) {
	var fence recoveryIntent
	var complete recoveryResult
	if r.HasIntent, err = readRecord(d, recoveryIntentFile, &r.Intent); err != nil {
		return
	}
	if r.HasFence, err = readRecord(d, recoveryFenceFile, &fence); err != nil {
		return
	}
	if r.HasResult, err = readRecord(d, recoveryResultFile, &r.Result); err != nil {
		return
	}
	if r.Complete, err = readRecord(d, recoveryCompleteFile, &complete); err != nil {
		return
	}
	if !r.HasIntent {
		if r.HasFence || r.HasResult || r.Complete {
			err = fmt.Errorf("Signal recovery lost its intent")
		}
		return
	}
	if (r.Intent.Version != RecoveryVersion && r.Intent.Version != 2) || (r.Intent.Version == 1 && r.Intent.Ancestor != "") || (r.Intent.Version == 2 && !safefs.DigestPattern.MatchString(r.Intent.Ancestor)) || r.Intent.Request.validate() != nil || r.Intent.DirectoryInode == 0 || r.Intent.LeaseInode == 0 {
		err = fmt.Errorf("invalid Signal recovery intent")
		return
	}
	actual, e := recoveryIdentity(d)
	if e != nil {
		return r, e
	}
	actual.Version, actual.Request, actual.Ancestor = r.Intent.Version, r.Intent.Request, r.Intent.Ancestor
	links, e := readHistory(d)
	if e != nil {
		return r, e
	}
	maintenance, e := readMaintenance(d)
	if e != nil {
		return r, e
	}
	actual.LinkHash, actual.MaintenanceHash = protocol.Hash(links), protocol.Hash(maintenance)
	if actual != r.Intent {
		return r, fmt.Errorf("Signal recovery directory, lease or account maintenance history changed")
	}
	if err = verifyRecoveryAncestors(d, r.Intent); err != nil {
		return r, err
	}
	if r.HasFence && fence != r.Intent || r.HasResult && !r.HasFence || r.Complete && !r.HasResult {
		return r, fmt.Errorf("Signal recovery records diverged")
	}
	if r.HasResult && (r.Result.Intent != protocol.Hash(r.Intent) || !safefs.DigestPattern.MatchString(r.Result.Result)) {
		return r, fmt.Errorf("invalid Signal recovery result")
	}
	if r.Complete && complete != r.Result {
		return r, fmt.Errorf("Signal recovery completion changed")
	}
	return
}
func requireNoRecovery(d *safefs.Directory) error {
	r, err := readRecovery(d)
	if err != nil {
		return err
	}
	if r.HasIntent {
		return fmt.Errorf("Signal account is fenced by recovery %s", r.Intent.Request.Operation)
	}
	return nil
}

// InspectRecovery is offline and does not acquire the account/CLI lease or
// repair partial records. Completion still means runtime_allowed=false.
func InspectRecovery(path string) (RecoveryStatus, error) {
	d, l, err := control(path, false)
	if err != nil {
		return RecoveryStatus{}, err
	}
	defer d.Close()
	defer l.Close()
	r, err := readRecovery(d)
	if err != nil {
		return RecoveryStatus{}, err
	}
	s := r.status()
	links, err := readHistory(d)
	if err != nil {
		return RecoveryStatus{}, err
	}
	maintenance, err := readMaintenance(d)
	if err != nil {
		return RecoveryStatus{}, err
	}
	if err = maintenanceLinkEvidence(maintenance, links); err != nil {
		return RecoveryStatus{}, err
	}
	s.RuntimeAllowed = s.RuntimeAllowed && links.runtimeAllowed() && maintenance.runtimeAllowed()
	return s, nil
}
