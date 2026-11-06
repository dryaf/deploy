package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runInstallationRetirementCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Explicit local operation")
	root := f.String("root", "", "Uninstalled predecessor root")
	manifest := f.String("manifest-hash", "", "Exact uninstalled manifest hash")
	target := f.String("target", "", "Private replacement target JSON")
	certificate := f.String("certificate", "", "Private completed retirement JSON")
	hash := f.String("certificate-hash", "", "Independently retained certificate hash")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("retirement/replacement requires --local and explicit paths")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	switch args[0] {
	case "installation-retire":
		if !managedPath(*root) || !managedPath(*target) || !safefs.DigestPattern.MatchString(*manifest) || *certificate != "" || *hash != "" {
			return fmt.Errorf("installation-retire requires root, manifest-hash and target only")
		}
		raw, err := nativeSignalRead(*target, safefs.MaxRecordBytes, true)
		if err != nil {
			return err
		}
		var t ReplacementTarget
		if err = safefs.DecodeJSON(raw, &t, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if err = t.validate(); err != nil {
			return err
		}
		if ui.DryRun {
			_, err = fmt.Fprintln(out, "[DRY] target validated; no installation opened or retired")
			return err
		}
		v, err := newManagedInstaller().retireInstallation(ctx, *root, *manifest, t)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Certificate InstallationRetirement `json:"certificate"`
			Hash        string                 `json:"hash"`
		}{v, protocol.Hash(v)})
	case "replacement-init":
		if !managedPath(*certificate) || !safefs.DigestPattern.MatchString(*hash) || *root != "" || *manifest != "" || *target != "" {
			return fmt.Errorf("replacement-init requires certificate and certificate-hash only")
		}
		raw, err := nativeSignalRead(*certificate, safefs.MaxRecordBytes, true)
		if err != nil {
			return err
		}
		var v InstallationRetirement
		if err = safefs.DecodeJSON(raw, &v, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if err = v.validate(); err != nil {
			return err
		}
		if protocol.Hash(v) != *hash || v.Target.UID != os.Getuid() {
			return fmt.Errorf("replacement certificate digest or UID mismatch")
		}
		if ui.DryRun {
			_, err = fmt.Fprintln(out, "[DRY] certificate validated; no replacement state initialized")
			return err
		}
		id, err := initializeReplacement(ctx, v, *hash)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(id)
	}
	return fmt.Errorf("unknown retirement operation")
}

func runReplacementTargetCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("replacement-target", flag.ContinueOnError)
	local := f.Bool("local", false, "Explicit local review")
	config := f.String("config", "", "Proposed private runtime JSON")
	root := f.String("root", "", "New installation root")
	state := f.String("state-dir", "", "New worker state directory")
	uid := f.Int("uid", -1, "New owner's numeric UID")
	reason := f.String("reason", "", "Audit reason")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) {
		return fmt.Errorf("replacement-target requires --local and an absolute runtime configuration")
	}
	raw, err := nativeSignalRead(*config, safefs.MaxRecordBytes, true)
	if err != nil {
		return err
	}
	var c Runtime
	err = safefs.DecodeJSON(raw, &c, safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	t := ReplacementTarget{1, *uid, *root, *state, c, *reason}
	if err = t.validate(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(t)
}
