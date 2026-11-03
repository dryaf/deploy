package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
	"golang.org/x/sys/unix"
)

func runManagedCLI(args []string) error {
	if args[0] == "__managed-signal-supervision-probe" || args[0] == "__managed-signal-verification-probe" || args[0] == "__managed-probe" || args[0] == "__managed-migration-probe" || args[0] == "__managed-led-migration-probe" || args[0] == "__managed-recovery-probe" || args[0] == "__managed-telegram-renewal-probe" || args[0] == "__managed-helper-upgrade-probe" {
		if len(args) != 1 {
			return fmt.Errorf("probe takes no arguments")
		}
		capability := managedCapabilities{Version: managedVersion, Schema: worker.StorageSchemaVersion()}
		if args[0] == "__managed-signal-verification-probe" || args[0] == "__managed-signal-supervision-probe" {
			capability.SignalVerification = 1
			if args[0] == "__managed-signal-supervision-probe" {
				capability.SignalSupervision = 1
			}
		}
		if args[0] == "__managed-migration-probe" {
			capability.SchemaMigration = 1
		}
		if args[0] == "__managed-led-migration-probe" {
			capability.LEDMappingMigration = 1
		}
		if args[0] == "__managed-recovery-probe" {
			capability.RecoveryVerification = 1
		}
		if args[0] == "__managed-telegram-renewal-probe" {
			capability.TelegramRenewal = 1
		}
		if args[0] == "__managed-helper-upgrade-probe" {
			capability.HelperUpgrade = 1
		}
		return json.NewEncoder(os.Stdout).Encode(capability)
	}
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use the current user's native verification installation")
	specPath := f.String("spec", "", "Private installation spec JSON")
	root := f.String("root", "", "Existing managed installation root")
	revision := f.String("revision", "", "Expected immutable revision (internal runtime only)")
	fromRevision := f.String("from-revision", "", "Retained executable revision to re-release; no state/schema rollback")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("managed operations require --local and explicit paths")
	}
	allowed := map[string]map[string]bool{
		"supervise-signal": {"local": true, "spec": true}, "adopt-signal": {"local": true, "spec": true}, "upgrade-helper": {"local": true, "spec": true}, "adopt-recovery": {"local": true, "spec": true}, "setup": {"local": true, "spec": true}, "update": {"local": true, "spec": true}, "migrate-schema": {"local": true, "spec": true}, "migrate-led": {"local": true, "spec": true},
		"resume": {"local": true, "root": true}, "uninstall": {"local": true, "root": true},
		"renew-telegram":      {"local": true, "root": true},
		"rollback":            {"local": true, "root": true, "from-revision": true},
		"installation-status": {"local": true, "root": true}, "__managed-serve": {"local": true, "root": true, "revision": true},
	}
	bad := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			bad = true
		}
	})
	if bad {
		return fmt.Errorf("option is not valid for managed operation")
	}
	var spec ManagedSpec
	if args[0] == "setup" || args[0] == "update" || args[0] == "migrate-schema" || args[0] == "migrate-led" || args[0] == "adopt-recovery" || args[0] == "upgrade-helper" || args[0] == "adopt-signal" || args[0] == "supervise-signal" {
		b, err := readManagedInput(*specPath, 1<<20)
		if err != nil {
			return err
		}
		if err = safefs.DecodeJSON(b, &spec, 1<<20); err != nil {
			return err
		}
		if err = spec.validate(); err != nil {
			return err
		}
		if (args[0] == "adopt-signal" || args[0] == "supervise-signal") != (spec.SignalVerification != nil) {
			return fmt.Errorf("adopt-signal and supervise-signal require signal_verification")
		}
		if (args[0] == "adopt-recovery") != (spec.RecoveryCommitHash != "") {
			return fmt.Errorf("only adopt-recovery requires recovery_commit_hash in its spec")
		}
		configHome, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		if spec.UnitDirectory != filepath.Join(configHome, "systemd", "user") {
			return fmt.Errorf("unit_directory must be this user's systemd configuration directory")
		}
	} else if !managedPath(*root) {
		return fmt.Errorf("root requires a clean absolute path")
	}
	if ui.DryRun {
		if args[0] == "rollback" {
			v, _, _, e := reviewManagedRollback(*root, *fromRevision)
			if e != nil {
				return e
			}
			return json.NewEncoder(os.Stdout).Encode(struct {
				DryRun bool            `json:"dry_run"`
				Review managedRollback `json:"review"`
			}{true, v})
		}
		if args[0] == "setup" || args[0] == "update" || args[0] == "migrate-schema" || args[0] == "migrate-led" || args[0] == "adopt-recovery" || args[0] == "upgrade-helper" || args[0] == "adopt-signal" || args[0] == "supervise-signal" {
			raw, e := readManagedInput(spec.RuntimeFile, safefs.MaxRecordBytes)
			if e != nil {
				return e
			}
			if _, e = decodeRuntime(raw); e != nil {
				return e
			}
			if spec.SignalVerification != nil {
				p, e := inspectSignalVerificationPublication(*spec.SignalVerification)
				if e != nil {
					return e
				}
				if managedDigest(raw) != p.NextGatewaySHA256 {
					return fmt.Errorf("staged Signal configuration pin changed")
				}
			}
			binary, e := readManagedInput(spec.BinaryFile, maxManagedBinary)
			if e != nil {
				return e
			}
			if managedDigest(binary) != spec.BinarySHA256 {
				return fmt.Errorf("input executable digest mismatch")
			}
			if e = checkManagedELF(binary); e != nil {
				return e
			}
			if _, e = RenderVerificationService(ServicePaths{Executable: filepath.Join(spec.Root, "releases", "preview", "deploy"), Config: filepath.Join(spec.Root, "releases", "preview", "runtime.json"), State: spec.State, Socket: filepath.Join(spec.State, "api.sock"), DeniedPaths: spec.DeniedPaths}); e != nil {
				return e
			}
		}
		fmt.Fprintln(os.Stdout, "[DRY] managed verification lifecycle: local inputs validated; no files, services, messages or LEDs changed; executable/manager/runtime checks deferred")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	i := newManagedInstaller()
	var m ManagedManifest
	var err error
	switch args[0] {
	case "renew-telegram":
		m, err = i.prepareTelegramRenewal(ctx, *root)
	case "setup", "update", "migrate-schema", "migrate-led", "adopt-recovery", "upgrade-helper", "adopt-signal", "supervise-signal":
		m, err = i.prepare(ctx, args[0], spec)
	case "resume":
		m, err = i.resume(ctx, *root)
	case "uninstall":
		m, err = i.uninstall(ctx, *root)
	case "rollback":
		m, err = i.rollback(ctx, *root, *fromRevision)
	case "installation-status":
		d, e := privateManagedDir(*root, true)
		if e != nil {
			return e
		}
		defer d.Close()
		lock, e := d.Lock("operation.lock", false, false)
		if e != nil {
			return e
		}
		defer lock.Close()
		j, e := readManagedJournal(d)
		if e != nil {
			return e
		}
		if j.Intent.After.Root != *root {
			return fmt.Errorf("installation root moved")
		}
		if e = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); e != nil {
			return e
		}
		if j.Intent.Operation == "renew-telegram" && managedPhase(j.Phase) < managedPhase("published") {
			_, e = verifyManagedArtifactsPendingTelegram(d, j.Intent.After, j.Intent.ID)
		} else {
			_, e = verifyManagedArtifacts(d, j.Intent.After)
		}
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Manifest  ManagedManifest `json:"manifest"`
			Operation string          `json:"operation"`
			Phase     string          `json:"phase"`
			Qualified bool            `json:"qualified"`
		}{j.Intent.After, j.Intent.Operation, j.Phase, false})
	case "__managed-serve":
		return serveManaged(ctx, *root, *revision)
	default:
		return fmt.Errorf("unknown managed operation")
	}
	if err != nil {
		return err
	}
	if args[0] == "renew-telegram" && err == nil {
		d, e := privateManagedDir(m.Root, true)
		if e != nil {
			return e
		}
		defer d.Close()
		lock, e := d.Lock("operation.lock", false, false)
		if e != nil {
			return e
		}
		defer lock.Close()
		receipt, e := readManagedTelegramReceipt(d, m)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Manifest ManagedManifest        `json:"manifest"`
			Receipt  managedTelegramReceipt `json:"receipt"`
		}{m, receipt})
	}
	return json.NewEncoder(os.Stdout).Encode(m)
}

// The shared lease spans the entire runtime. A journal can authorize only this
// exact verification revision; a file/config swap cannot enable production.
func serveManaged(ctx context.Context, root, revision string) error {
	if !safefs.ValidID(revision, "n") {
		return fmt.Errorf("invalid managed revision")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return err
	}
	m := j.Intent.After
	if m.Root != root || m.Revision != revision || m.Mode != "verification" || managedPhase(j.Phase) < managedPhase("published") {
		return fmt.Errorf("installation is not ready for verification service")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return err
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return err
	}
	if j.Intent.Operation == "migrate-schema" {
		if err = worker.VerifyManagedSchemaMigration(ctx, m.Paths.State, c.Worker, m.Identity, j.Intent.ID, j.Intent.Before.Schema); err != nil {
			return err
		}
	}
	if j.Intent.Operation == "migrate-led" {
		if err = verifyManagedLEDCompletion(ctx, d, j, c); err != nil {
			return err
		}
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return err
	}
	if !present || managedDigest(unit) != m.UnitHash {
		return fmt.Errorf("managed unit changed")
	}
	// os.Executable alone is insufficient: compare the running inode to the
	// opened pinned release, including an executable replaced after startup.
	running, err := os.Stat("/proc/self/exe")
	if err != nil {
		return err
	}
	pinned, err := d.Open("releases/"+revision+"/deploy", unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, err := pinned.Stat()
	pinned.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(running, info) {
		return fmt.Errorf("managed runtime is not the pinned executable")
	}
	if err = verifyManagedTelegramAudit(ctx, d, m, c); err != nil {
		return err
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.Identity() != m.Identity {
		return fmt.Errorf("managed state identity changed")
	}
	if err = s.RequireInactiveProducers(); err != nil {
		return err
	}
	return serveManagedSignalRuntime(ctx, d, s, c, m)
}
