package install

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type managedCapabilities struct {
	LegacyOwnership      int `json:"legacy_ownership,omitempty"`
	Delivery             int `json:"delivery,omitempty"`
	SignalProfiles       int `json:"signal_profiles,omitempty"`
	SignalService        int `json:"signal_service,omitempty"`
	SignalSupervision    int `json:"signal_supervision,omitempty"`
	SignalVerification   int `json:"signal_verification,omitempty"`
	HelperUpgrade        int `json:"helper_upgrade,omitempty"`
	TelegramRenewal      int `json:"telegram_renewal,omitempty"`
	RecoveryVerification int `json:"recovery_verification,omitempty"`
	Version              int `json:"version"`
	Schema               int `json:"schema"`
	SchemaMigration      int `json:"schema_migration,omitempty"`
	LEDMappingMigration  int `json:"led_mapping_migration,omitempty"`
}

func ownedManagedFile(d *safefs.Directory, name string, limit int64) ([]byte, error) {
	f, err := d.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if i.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || i.Size() > limit {
		return nil, fmt.Errorf("managed input has unsafe owner or size")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("managed input exceeds limit")
	}
	return b, err
}

func readManagedInput(path string, limit int64) ([]byte, error) {
	if !managedPath(path) {
		return nil, fmt.Errorf("invalid managed input path")
	}
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return ownedManagedFile(d, filepath.Base(path), limit)
}

// frozenManagedFile publishes an immutable, hash-checked file. Partial staging
// files can be abandoned after a crash; a published release is never overwritten.
func frozenManagedFile(d *safefs.Directory, name string, body []byte, mode uint32) error {
	old, err := ownedManagedFile(d, name, maxManagedBinary)
	if err == nil {
		if !bytes.Equal(old, body) {
			return fmt.Errorf("immutable installation artifact changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id
	f, err := d.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(d.File().Fd()), tmp, 0)
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.Rename(tmp, d, name)
}

func checkManagedELF(body []byte) error {
	f, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("managed executable must be a native Linux ELF binary: %w", err)
	}
	defer f.Close()
	machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if machine == 0 || f.Machine != machine || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("unsupported executable architecture")
	}
	return nil
}

// Execute the already-open, pinned inode, not a path that may have been replaced
// between validation and exec. The child is a trusted user-selected binary.
func probeManagedBinary(ctx context.Context, d *safefs.Directory, name, hash string, migration bool) error {
	probe := "__managed-probe"
	if migration {
		probe = "__managed-migration-probe"
	}
	return probeManagedCapability(ctx, d, name, hash, probe)
}

func probeManagedLEDMapping(ctx context.Context, d *safefs.Directory, name, hash string) error {
	return probeManagedCapability(ctx, d, name, hash, "__managed-led-migration-probe")
}

func probeManagedCapability(ctx context.Context, d *safefs.Directory, name, hash, probe string) error {
	f, err := d.Open(name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	if i.Size() > maxManagedBinary || i.Mode().Perm()&0100 == 0 || i.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("unsafe managed executable")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxManagedBinary+1))
	if err != nil {
		return err
	}
	if managedDigest(b) != hash {
		return fmt.Errorf("managed executable digest mismatch")
	}
	if err = checkManagedELF(b); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "notification-worker", probe)
	cmd.ExtraFiles = []*os.File{f}
	out, err := process.LimitedOutput(cmd, 10*time.Second, 4096)
	if err != nil {
		return err
	}
	var c managedCapabilities
	if err = safefs.DecodeJSON(out, &c, 4096); err != nil {
		return err
	}
	if c.Version != managedVersion || c.Schema != worker.StorageSchemaVersion() ||
		(probe == "__managed-legacy-probe" && c.LegacyOwnership != 1) ||
		(probe == "__managed-migration-probe" && c.SchemaMigration != 1) ||
		(probe == "__managed-led-migration-probe" && c.LEDMappingMigration != 1) ||
		(probe == "__managed-recovery-probe" && c.RecoveryVerification != 1) ||
		(probe == "__managed-helper-upgrade-probe" && c.HelperUpgrade != 1) ||
		(probe == "__managed-telegram-renewal-probe" && c.TelegramRenewal != 1) ||
		(probe == "__managed-signal-verification-probe" && c.SignalVerification != 1) ||
		(probe == "__managed-signal-supervision-probe" && (c.SignalSupervision != 1 || c.SignalVerification != 1)) ||
		(probe == "__managed-signal-profiles-probe" && c.SignalProfiles != 1) ||
		(probe == "__managed-delivery-probe" && c.Delivery != 1) ||
		(probe == "__signal-verification-service-probe" && c.SignalService != 1) {
		return fmt.Errorf("executable cannot safely resume this installation/schema")
	}
	return nil
}

func managedRuntime(d *safefs.Directory, m ManagedManifest) (Runtime, error) {
	var c Runtime
	b, err := ownedManagedFile(d, "releases/"+m.Revision+"/runtime.json", safefs.MaxRecordBytes)
	if err != nil {
		return c, err
	}
	if managedDigest(b) != m.RuntimeHash {
		return c, fmt.Errorf("runtime digest changed")
	}
	c, err = decodeRuntime(b)
	if err == nil && protocol.Hash(c) != m.ConfigHash {
		err = fmt.Errorf("resolved runtime changed")
	}
	return c, err
}

func verifyManagedArtifacts(d *safefs.Directory, m ManagedManifest) (Runtime, error) {
	return verifyManagedArtifactsVersion(d, m, false)
}
func verifyManagedArtifactsVersion(d *safefs.Directory, m ManagedManifest, migrationSource bool) (Runtime, error) {
	return verifyManagedArtifactsPolicy(d, m, migrationSource, "")
}
func verifyManagedArtifactsPendingTelegram(d *safefs.Directory, m ManagedManifest, id string) (Runtime, error) {
	if id == "" || m.TelegramRenewalID != id {
		return Runtime{}, fmt.Errorf("invalid pending Telegram operation")
	}
	return verifyManagedArtifactsPolicy(d, m, false, id)
}
func verifyManagedArtifactsPolicy(d *safefs.Directory, m ManagedManifest, migrationSource bool, pendingTelegram string) (Runtime, error) {
	var c Runtime
	if err := m.validate(); err != nil {
		return c, err
	}
	if m.Schema != worker.StorageSchemaVersion() && !(migrationSource && m.Schema == 9) {
		return c, fmt.Errorf("unsupported installed schema; migration required")
	}
	for name, hash := range map[string]string{"releases/" + m.Revision + "/deploy": m.BinaryHash, managedHelperName(m): m.HelperHash} {
		b, err := ownedManagedFile(d, name, maxManagedBinary)
		if err != nil {
			return c, err
		}
		if managedDigest(b) != hash {
			return c, fmt.Errorf("pinned installation executable changed")
		}
		if err = checkManagedELF(b); err != nil {
			return c, err
		}
	}
	c, err := managedRuntime(d, m)
	if err != nil {
		return c, err
	}
	if m.DeliveryOwnerHash != "" {
		if err := deliveryPolicy(m, c); err != nil {
			return c, err
		}
		if protocol.Hash(deliveryOwner(m, c)) != m.DeliveryOwnerHash {
			return c, fmt.Errorf("delivery owner hash changed")
		}
	}
	if m.DeliveryActivation != nil {
		if _, _, err := readDeliveryAdmission(d, m, c); err != nil {
			return c, err
		}
	}
	if m.SignalProfiles != nil {
		if _, e := managedEffectiveRuntime(d, m, c); e != nil {
			return c, e
		}
	}
	if m.SignalVerification != nil {
		if _, e := readManagedSignalBinding(d, m); e != nil {
			return c, e
		}
	}
	if m.TelegramRenewalID != "" {
		if c.Telegram == nil {
			return c, fmt.Errorf("managed Telegram configuration missing")
		}
		if pendingTelegram != "" {
			_, e := ownedManagedFile(d, "operations/"+pendingTelegram+"/telegram-receipt.json", 32768)
			if os.IsNotExist(e) {
				return c, nil
			}
			if e != nil {
				return c, e
			}
		}
		receipt, e := readManagedTelegramReceipt(d, m)
		if e != nil {
			return c, e
		}
		if receipt.Renewal.Binding.Account != c.Telegram.Account {
			return c, fmt.Errorf("managed Telegram receipt account changed")
		}
	}
	return c, nil
}

func readManagedJournal(d *safefs.Directory) (managedJournal, error) {
	var j managedJournal
	if err := d.ReadJSON("lifecycle.json", &j); err != nil {
		return j, err
	}
	if err := j.validate(); err != nil {
		return j, err
	}
	var intent managedIntent
	if err := d.ReadJSON("operations/"+j.Intent.ID+"/intent.json", &intent); err != nil {
		return j, err
	}
	if protocol.Hash(intent) != protocol.Hash(j.Intent) {
		return j, fmt.Errorf("installation journal differs from immutable intent")
	}
	if prior := j.Intent.Supersedes; prior != nil {
		var frozen managedIntent
		if err := d.ReadJSON("operations/"+prior.ID+"/intent.json", &frozen); err != nil {
			return j, err
		}
		if protocol.Hash(frozen) != protocol.Hash(*prior) {
			return j, fmt.Errorf("interrupted installation intent changed")
		}
	}
	return j, nil
}

func checkManagedDirectoryPath(d *safefs.Directory, path string, private bool) error {
	current, err := privateManagedDir(path, private)
	if err != nil {
		return err
	}
	defer current.Close()
	want, err := d.File().Stat()
	if err != nil {
		return err
	}
	got, err := current.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(want, got) {
		return fmt.Errorf("managed directory was replaced during operation")
	}
	return nil
}

func writeManagedIntent(d *safefs.Directory, j managedJournal) error {
	if err := j.validate(); err != nil {
		return err
	}
	if err := d.Mkdir("operations/" + j.Intent.ID); err != nil {
		return err
	}
	if err := d.WriteJSON("operations/"+j.Intent.ID+"/intent.json", j.Intent, false); err != nil {
		return err
	}
	return d.WriteJSON("lifecycle.json", j, true)
}
