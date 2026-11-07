package signalaccount

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const nativeOwnerFile = "deploy-native-owner.json"
const nativeOwnerComplete = "deploy-native-owner-complete.json"
const nativeOwnerLock = "deploy-native-owner.lock"

// NativeServiceBinding is first-install account ownership, separate from a
// recovered account. It pins the real completed native link and the supervising
// service's manifest. It contains neither a private account selector nor a URI.
type NativeServiceBinding struct {
	Version       int    `json:"version"`
	ServiceRoot   string `json:"service_root"`
	ServiceHash   string `json:"service_hash"`
	Account       string `json:"account"`
	LinkOperation string `json:"link_operation"`
	LinkHash      string `json:"link_hash"`
	CLISHA256     string `json:"cli_sha256"`
	CLIVersion    string `json:"cli_version"`
}
type nativeOwnerIntent struct {
	Version  int                  `json:"version"`
	Binding  NativeServiceBinding `json:"binding"`
	Complete bool                 `json:"complete"`
}

func (b NativeServiceBinding) validate(path string) error {
	if b.Version != 1 || !filepath.IsAbs(b.ServiceRoot) || filepath.Clean(b.ServiceRoot) != b.ServiceRoot || b.ServiceRoot == "/" || strings.ContainsAny(b.ServiceRoot, "\x00\r\n") || b.ServiceRoot == path || strings.HasPrefix(b.ServiceRoot, path+"/") || strings.HasPrefix(path, b.ServiceRoot+"/") || !protocol.ValidToken(b.Account) || !safefs.ValidID(b.LinkOperation, "n") || b.CLIVersion != "0.14.9" {
		return fmt.Errorf("invalid fresh Signal service binding")
	}
	for _, hash := range []string{b.ServiceHash, b.LinkHash, b.CLISHA256} {
		if !safefs.DigestPattern.MatchString(hash) {
			return fmt.Errorf("native Signal service binding requires exact evidence and executable pins")
		}
	}
	return nil
}

func nativeServicePresent(d *safefs.Directory) (bool, error) {
	for _, name := range []string{nativeOwnerLock, nativeOwnerFile, nativeOwnerComplete} {
		f, err := d.Open(name, os.O_RDONLY, 0)
		if err == nil {
			f.Close()
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}
func requireNoNativeService(d *safefs.Directory) error {
	present, err := nativeServicePresent(d)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("Signal account is owned by its native managed service; use its ownership lifecycle")
	}
	return nil
}

func requireNativeLink(d *safefs.Directory, b NativeServiceBinding) error {
	if err := requireNoAccountRecovery(d); err != nil {
		return err
	}
	h, err := readHistory(d)
	if err != nil {
		return err
	}
	if h.Pending || len(h.Records) == 0 || !h.runtimeAllowed() {
		return fmt.Errorf("native Signal service requires acknowledged completed linking")
	}
	last := h.Records[len(h.Records)-1]
	if last.Intent.ID != b.LinkOperation || protocol.Hash(last) != b.LinkHash || last.Intent.Native == nil || last.Intent.Native.SHA256 != b.CLISHA256 || last.Intent.Native.Version != b.CLIVersion {
		return fmt.Errorf("native Signal service link or executable changed")
	}
	g, err := readMaintenance(d)
	if err != nil {
		return err
	}
	if !g.runtimeAllowed() {
		return fmt.Errorf("finish Signal maintenance before binding native service")
	}
	return maintenanceLinkEvidence(g, h)
}

// BindNativeService permanently fences ordinary relays/linking before allowing
// supervised runtime. Interrupted publication resumes the same binding. Losing
// completed ownership evidence never permits a second account owner.
func BindNativeService(path string, b NativeServiceBinding) error {
	return bindNativeService(path, b, nil)
}
func bindNativeService(path string, b NativeServiceBinding, fault func(string) error) error {
	if err := b.validate(path); err != nil {
		return err
	}
	d, err := accountDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	present, err := nativeServicePresent(d)
	if err != nil {
		return err
	}
	op, err := d.Lock("deploy-service-link.lock", true, !present)
	if err != nil {
		return err
	}
	defer op.Close()
	guard, err := d.Lock("deploy-account-control.lock", true, false)
	if err != nil {
		return err
	}
	defer guard.Close()
	account, err := d.Lock("deploy-account.lock", true, false)
	if err != nil {
		return fmt.Errorf("Signal account is busy; stop its current runtime before binding")
	}
	defer account.Close()
	if err = requireNativeLink(d, b); err != nil {
		return err
	}
	claimed := false
	if f, e := d.Open(nativeOwnerLock, os.O_RDONLY, 0); e == nil {
		claimed = true
		f.Close()
	} else if !os.IsNotExist(e) {
		return e
	}
	var intent nativeOwnerIntent
	exists, err := readRecord(d, nativeOwnerFile, &intent)
	if err != nil {
		return err
	}
	var completed NativeServiceBinding
	hasComplete, err := readRecord(d, nativeOwnerComplete, &completed)
	if err != nil {
		return err
	}
	if !claimed && (exists || hasComplete) || claimed && !exists {
		return fmt.Errorf("native Signal ownership authority is missing; recovery review required")
	}
	if !exists {
		lease, e := d.Lock(nativeOwnerLock, true, true)
		if e != nil {
			return e
		}
		defer lease.Close()
		intent = nativeOwnerIntent{Version: 1, Binding: b}
		if err = writeRecord(d, nativeOwnerFile, intent, false); err != nil {
			return err
		}
	} else if intent.Version != 1 || intent.Binding != b || hasComplete && completed != b || intent.Complete && !hasComplete {
		return fmt.Errorf("native Signal ownership binding changed or completion was lost")
	}
	if fault != nil {
		if err = fault("intent"); err != nil {
			return err
		}
	}
	if !hasComplete {
		if err = writeRecord(d, nativeOwnerComplete, b, false); err != nil {
			return err
		}
	}
	if fault != nil {
		if err = fault("receipt"); err != nil {
			return err
		}
	}
	if !intent.Complete {
		intent.Complete = true
		if err = writeRecord(d, nativeOwnerFile, intent, true); err != nil {
			return err
		}
	}
	if fault != nil {
		return fault("complete")
	}
	return nil
}

func checkNativeBinding(d *safefs.Directory, path string, b NativeServiceBinding) error {
	if err := b.validate(path); err != nil {
		return err
	}
	var intent nativeOwnerIntent
	present, err := readRecord(d, nativeOwnerFile, &intent)
	if err != nil {
		return err
	}
	if !present || intent.Version != 1 || intent.Binding != b || !intent.Complete {
		return fmt.Errorf("native Signal service ownership is incomplete or changed")
	}
	var complete NativeServiceBinding
	present, err = readRecord(d, nativeOwnerComplete, &complete)
	if err != nil {
		return err
	}
	if !present || complete != b {
		return fmt.Errorf("native Signal service completion is missing or changed")
	}
	return requireNativeLink(d, b)
}

// InspectNativeService checks immutable account ownership while its service may
// hold the account lease. It neither links nor repairs missing records.
func InspectNativeService(path string, b NativeServiceBinding) error {
	d, err := accountDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	for _, name := range []string{"deploy-service-link.lock", nativeOwnerLock, "deploy-account-control.lock"} {
		lease, e := d.Lock(name, false, false)
		if e != nil {
			return e
		}
		defer lease.Close()
	}
	return checkNativeBinding(d, path, b)
}

type NativeRuntime struct {
	mu                        sync.Mutex
	path                      string
	dir                       *safefs.Directory
	operation, owner, account *safefs.Lock
	controlIdentity           os.FileInfo
	binding                   NativeServiceBinding
	admission                 func() error
}

// OpenNativeRuntime keeps the actual linked account leased, without recovery
// records. Native CLI children inherit the same descriptor until they exit.
func OpenNativeRuntime(path string, b NativeServiceBinding, admission func() error) (_ *NativeRuntime, err error) {
	if admission == nil || b.validate(path) != nil {
		return nil, fmt.Errorf("native Signal service admission and binding required")
	}
	if err = admission(); err != nil {
		return nil, err
	}
	r := &NativeRuntime{path: path, binding: b, admission: admission}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if r.dir, err = accountDirectory(path); err != nil {
		return nil, err
	}
	if r.operation, err = r.dir.Lock("deploy-service-link.lock", false, false); err != nil {
		return nil, err
	}
	if r.owner, err = r.dir.Lock(nativeOwnerLock, false, false); err != nil {
		return nil, err
	}
	if r.account, err = r.dir.Lock("deploy-account.lock", true, false); err != nil {
		return nil, err
	}
	if err = r.Check(); err != nil {
		return nil, err
	}
	return r, nil
}
func (r *NativeRuntime) check() error {
	if r == nil || r.dir == nil || r.account == nil {
		return fmt.Errorf("native Signal runtime is closed")
	}
	if err := r.admission(); err != nil {
		return err
	}
	d, err := accountDirectory(r.path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = sameAccountDirectory(r.dir, d); err != nil {
		return err
	}
	guard, err := d.Lock("deploy-account-control.lock", false, false)
	if err != nil {
		return err
	}
	defer guard.Close()
	info, err := guard.File().Stat()
	if err != nil {
		return err
	}
	if r.controlIdentity != nil && !os.SameFile(info, r.controlIdentity) {
		return fmt.Errorf("native Signal control lease replaced")
	}
	r.controlIdentity = info
	for name, lease := range map[string]*safefs.Lock{"deploy-service-link.lock": r.operation, nativeOwnerLock: r.owner, "deploy-account.lock": r.account, "deploy-account-control.lock": guard} {
		if err = sameAccountLease(d, name, lease); err != nil {
			return err
		}
	}
	return checkNativeBinding(d, r.path, r.binding)
}
func (r *NativeRuntime) Check() error {
	if r == nil {
		return fmt.Errorf("native Signal runtime is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.check()
}
func (r *NativeRuntime) FileFor(path string) (*os.File, error) {
	if r == nil {
		return nil, fmt.Errorf("native Signal runtime is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return nil, err
	}
	if path != "" && path != r.path {
		return nil, fmt.Errorf("native Signal command account directory changed")
	}
	return r.account.File(), nil
}

func (r *NativeRuntime) AccountIdentity() (string, error) {
	if r == nil {
		return "", fmt.Errorf("native Signal runtime is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return "", err
	}
	return r.binding.Account, nil
}
func (r *NativeRuntime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account != nil {
		r.account.Close()
		r.account = nil
	}
	if r.owner != nil {
		r.owner.Close()
		r.owner = nil
	}
	if r.operation != nil {
		r.operation.Close()
		r.operation = nil
	}
	if r.dir != nil {
		r.dir.Close()
		r.dir = nil
	}
}
