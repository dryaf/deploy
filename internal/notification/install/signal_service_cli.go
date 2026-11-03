package install

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runSignalServiceCLI(args []string, out io.Writer) error {
	if args[0] == "__signal-verification-service-probe" {
		if len(args) != 1 {
			return fmt.Errorf("Signal service probe takes no arguments")
		}
		return json.NewEncoder(out).Encode(managedCapabilities{Version: managedVersion, Schema: worker.StorageSchemaVersion(), SignalService: 1})
	}
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Manage a separate local Signal verification service")
	specPath := f.String("spec", "", "Private Signal verifier service spec")
	root := f.String("root", "", "Existing Signal service installation root")
	operation := f.String("operation", "", "Pinned operation (internal runtime)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("Signal service operations require --local")
	}
	allowed := map[string]bool{"local": true}
	if args[0] == "signal-service-setup" {
		allowed["spec"] = true
	} else {
		allowed["root"] = true
	}
	if args[0] == "__signal-service-serve" {
		allowed["operation"] = true
	}
	bad := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[v.Name] {
			bad = true
		}
	})
	if bad {
		return fmt.Errorf("invalid Signal service option")
	}
	var s SignalServiceSpec
	if args[0] == "signal-service-setup" {
		b, err := nativeSignalRead(*specPath, safefs.MaxRecordBytes, true)
		if err != nil {
			return err
		}
		if err = safefs.DecodeJSON(b, &s, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if err = s.validate(); err != nil {
			return err
		}
		home, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		if s.UnitDirectory != filepath.Join(home, "systemd", "user") {
			return fmt.Errorf("Signal unit directory must be this user's systemd directory")
		}
	} else if !managedPath(*root) {
		return fmt.Errorf("Signal service root must be absolute")
	}
	if args[0] == "__signal-service-serve" && !safefs.ValidID(*operation, "n") {
		return fmt.Errorf("Signal service operation pin required")
	}
	if ui.DryRun {
		if args[0] == "signal-service-setup" {
			if _, err := inspectSignalVerificationPublication(s.Endpoint); err != nil {
				return err
			}
			b, err := nativeSignalRead(s.BinaryFile, maxManagedBinary, false)
			if err != nil {
				return err
			}
			if managedDigest(b) != s.BinarySHA256 {
				return fmt.Errorf("Signal binary hash changed")
			}
			if err = checkManagedELF(b); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(out, "[DRY] Signal service inputs validated; no service, account lease, socket or file changed")
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "__signal-service-serve" {
		err := serveSignalService(ctx, *root, *operation)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	i := newSignalServiceInstaller()
	var m SignalServiceManifest
	var err error
	switch args[0] {
	case "signal-service-setup":
		m, err = i.setup(ctx, s)
	case "signal-service-resume":
		m, err = i.resume(ctx, *root)
	case "signal-service-uninstall":
		m, err = i.uninstall(ctx, *root)
	case "signal-service-inspect":
		d, l, e := openSignalServiceRoot(*root, false)
		if e != nil {
			return e
		}
		defer d.Close()
		defer l.Close()
		m, e = readSignalService(d, "intent.json")
		if e != nil {
			return e
		}
		if m.Spec.Root != *root {
			return fmt.Errorf("Signal service root changed")
		}
		j, e := readSignalServiceJournal(d, m)
		if e != nil {
			return e
		}
		if e = checkSignalServiceFiles(d, m, j.Phase != "removed" && signalServicePhase(j.Phase) >= signalServicePhase("published")); e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(struct {
			Manifest SignalServiceManifest `json:"manifest"`
			Phase    string                `json:"phase"`
		}{m, j.Phase})
	default:
		return fmt.Errorf("unknown Signal service operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(m)
}
