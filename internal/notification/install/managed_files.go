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
	SharedActivity              int `json:"shared_activity,omitempty"`
	SignalDelivery              int `json:"signal_delivery,omitempty"`
	CommandProviders            int `json:"command_providers,omitempty"`
	CommandRoutes               int `json:"command_routes,omitempty"`
	CommandProducer             int `json:"command_producer,omitempty"`
	CommandFreshness            int `json:"command_freshness,omitempty"`
	CommandLimits               int `json:"command_limits,omitempty"`
	CommandPause                int `json:"command_pause,omitempty"`
	DeliveryLimits              int `json:"delivery_limits,omitempty"`
	DeliveryProviders           int `json:"delivery_providers,omitempty"`
	DeliveryPause               int `json:"delivery_pause,omitempty"`
	CommandGrants               int `json:"command_grants,omitempty"`
	BootstrapProviders          int `json:"bootstrap_providers,omitempty"`
	FreshSignalRestart          int `json:"fresh_signal_restart,omitempty"`
	BootstrapEnrollmentLED      int `json:"bootstrap_enrollment_led,omitempty"`
	BootstrapEnrollmentCommands int `json:"bootstrap_enrollment_commands,omitempty"`
	EnrolledLimits              int `json:"enrolled_limits,omitempty"`
	BootstrapLimits             int `json:"bootstrap_limits,omitempty"`
	LEDEndpoint                 int `json:"led_endpoint,omitempty"`
	LEDOutput                   int `json:"led_output,omitempty"`
	LEDDeliveryOutput           int `json:"led_delivery_output,omitempty"`
	FreshSignalGateway          int `json:"fresh_signal_gateway,omitempty"`
	FreshSignal                 int `json:"fresh_signal,omitempty"`
	Bootstrap                   int `json:"bootstrap,omitempty"`
	SignalNextRecovery          int `json:"signal_next_recovery,omitempty"`
	NextRecovery                int `json:"next_recovery,omitempty"`
	LEDDelivery                 int `json:"led_delivery,omitempty"`
	SignalConversations         int `json:"signal_conversations,omitempty"`
	SignalProduction            int `json:"signal_production,omitempty"`
	CommandRecovery             int `json:"command_recovery,omitempty"`
	TelegramHandoff             int `json:"telegram_handoff,omitempty"`
	Commands                    int `json:"commands,omitempty"`
	CommandEnrollment           int `json:"command_enrollment,omitempty"`
	CommandPolicy               int `json:"command_policy,omitempty"`
	DeliveryRecovery            int `json:"delivery_recovery,omitempty"`
	LegacyOwnership             int `json:"legacy_ownership,omitempty"`
	Delivery                    int `json:"delivery,omitempty"`
	SignalProfiles              int `json:"signal_profiles,omitempty"`
	SignalService               int `json:"signal_service,omitempty"`
	SignalReplacement           int `json:"signal_replacement,omitempty"`
	SignalSupervision           int `json:"signal_supervision,omitempty"`
	SignalVerification          int `json:"signal_verification,omitempty"`
	HelperUpgrade               int `json:"helper_upgrade,omitempty"`
	TelegramRenewal             int `json:"telegram_renewal,omitempty"`
	RecoveryVerification        int `json:"recovery_verification,omitempty"`
	Version                     int `json:"version"`
	Schema                      int `json:"schema"`
	SchemaMigration             int `json:"schema_migration,omitempty"`
	LEDMappingMigration         int `json:"led_mapping_migration,omitempty"`
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
		(probe == "__managed-shared-activity-probe" && c.SharedActivity != 1) ||
		(probe == "__managed-delivery-limits-probe" && c.DeliveryLimits != 5) ||
		(probe == "__managed-signal-delivery-probe" && c.SignalDelivery != 1) ||
		(probe == "__managed-delivery-providers-probe" && c.DeliveryProviders != 1) ||
		(probe == "__managed-delivery-pause-probe" && c.DeliveryPause != 1) ||
		(probe == "__managed-command-providers-probe" && c.CommandProviders != 1) ||
		(probe == "__managed-command-routes-probe" && c.CommandRoutes != 1) ||
		(probe == "__managed-command-producer-probe" && c.CommandProducer != 1) ||
		(probe == "__managed-command-freshness-probe" && c.CommandFreshness != 1) ||
		(probe == "__managed-command-limits-probe" && c.CommandLimits != 1) ||
		(probe == "__managed-command-pause-probe" && c.CommandPause != 1) ||
		(probe == "__fresh-signal-probe" && c.FreshSignal != 1) ||
		(probe == "__fresh-signal-restart-probe" && c.FreshSignalRestart != 1) ||
		(probe == "__managed-fresh-signal-probe" && c.FreshSignalGateway != 1) ||
		(probe == "__managed-enrolled-limits-probe" && c.EnrolledLimits != 16) ||
		(probe == "__managed-bootstrap-limits-probe" && c.BootstrapLimits != 3) ||
		(probe == "__managed-bootstrap-providers-probe" && c.BootstrapProviders != 2) ||
		(probe == "__managed-bootstrap-probe" && c.Bootstrap != 1) ||
		(probe == "__managed-bootstrap-enrollment-led-probe" && c.BootstrapEnrollmentLED != 1) ||
		(probe == "__managed-bootstrap-enrollment-commands-probe" && c.BootstrapEnrollmentCommands != 1) ||
		(probe == "__managed-signal-replacement-probe" && c.SignalReplacement != 1) ||
		(probe == "__managed-signal-production-probe" && (c.SignalProduction != 1 || c.SignalReplacement != 1)) ||
		(probe == "__managed-signal-conversations-probe" && (c.SignalConversations != 1 || c.SignalProfiles != 1)) ||
		(probe == "__signal-verification-replacement-probe" && c.SignalReplacement != 1) ||
		(probe == "__managed-command-recovery-probe" && c.CommandRecovery != 1) ||
		(probe == "__managed-telegram-handoff-probe" && c.TelegramHandoff != 1) ||
		(probe == "__managed-commands-probe" && c.Commands != 1) ||
		(probe == "__managed-command-grants-probe" && c.CommandGrants != 1) ||
		(probe == "__managed-command-policy-probe" && c.CommandPolicy != 1) ||
		(probe == "__managed-command-enrollment-probe" && c.CommandEnrollment != 1) ||
		(probe == "__managed-led-endpoint-probe" && c.LEDEndpoint != 1) ||
		(probe == "__managed-led-output-probe" && c.LEDOutput != 1) ||
		(probe == "__managed-delivery-led-output-probe" && c.LEDDeliveryOutput != 1) ||
		(probe == "__managed-led-delivery-probe" && c.LEDDelivery != 1) ||
		(probe == "__managed-delivery-recovery-probe" && c.DeliveryRecovery != 1) ||
		(probe == "__managed-legacy-probe" && c.LegacyOwnership != 1) ||
		(probe == "__managed-migration-probe" && c.SchemaMigration != 1) ||
		(probe == "__managed-led-migration-probe" && c.LEDMappingMigration != 2) ||
		(probe == "__managed-next-recovery-probe" && c.NextRecovery != 1) ||
		(probe == "__signal-next-recovery-probe" && c.SignalNextRecovery != 1) ||
		(probe == "__managed-recovery-probe" && c.RecoveryVerification != 1) ||
		(probe == "__managed-helper-upgrade-probe" && c.HelperUpgrade != 1) ||
		(probe == "__managed-telegram-renewal-probe" && c.TelegramRenewal != 1) ||
		(probe == "__managed-signal-verification-probe" && c.SignalVerification != 1) ||
		(probe == "__managed-signal-supervision-probe" && (c.SignalSupervision != 1 || c.SignalVerification != 1)) ||
		(probe == "__managed-signal-profiles-probe" && c.SignalProfiles != 1) ||
		(probe == "__managed-delivery-probe" && c.Delivery != 1) ||
		(probe == "__signal-production-service-probe" && (c.SignalProduction != 1 || c.SignalService != 1 || c.SignalReplacement != 1)) ||
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
	return verifyManagedArtifactsPolicy(d, m, migrationSource, "", nil)
}
func verifyManagedArtifactsPendingTelegram(d *safefs.Directory, m ManagedManifest, id string) (Runtime, error) {
	if id == "" || m.TelegramRenewalID != id {
		return Runtime{}, fmt.Errorf("invalid pending Telegram operation")
	}
	return verifyManagedArtifactsPolicy(d, m, false, id, nil)
}
func verifyManagedArtifactsPolicy(d *safefs.Directory, m ManagedManifest, migrationSource bool, pendingTelegram string, successor *SignalVerificationReference) (Runtime, error) {
	if err := verifySharedActivityRestoration(d, m); err != nil {
		return Runtime{}, err
	}
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
	if m.FreshSignal != nil {
		if m.Mode == "uninstalled" {
			var service FreshSignalManifest
			service, _, err = inspectRetainedFreshSignal(*m.FreshSignal)
			if err == nil {
				err = validateFreshSignalGatewayManifest(service, m.Root, m.Paths.State, m.UnitDirectory, c)
			}
		} else {
			_, err = validateFreshSignalGateway(context.Background(), *m.FreshSignal, m.Root, m.Paths.State, m.UnitDirectory, c, false)
		}
		if err != nil {
			return c, err
		}
	}
	if c.Providers["signal"].SignalDelivery != nil && m.ReportCredentialHistory == nil {
		return c, fmt.Errorf("native recipient configuration lost its credential history anchor")
	}
	if m.ReportCredentialHistory != nil {
		if _, err := deliveryCredentialAncestry(d, managedJournal{Intent: managedIntent{After: m}}, c); err != nil {
			return c, err
		}
	}
	if m.DeliveryOwnerHash != "" {
		if err := deliveryPolicyWithSuccessor(m, c, successor); err != nil {
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
	if m.Commands != nil {
		if _, err := readManagedCommandAdmission(d, m, c); err != nil {
			return c, err
		}
	}
	if m.SignalProfiles != nil {
		if _, e := managedEffectiveRuntimePolicy(d, m, c, successor); e != nil {
			return c, e
		}
	}
	if m.SignalVerification != nil {
		if _, e := readManagedSignalBindingPolicy(d, m, successor); e != nil {
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
		receipt, e := readManagedTelegramReceiptPolicy(d, m, successor)
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
