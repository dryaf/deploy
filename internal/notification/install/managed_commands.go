package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/worker"
	"io"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func isManagedCommandPolicyOperation(op string) bool {
	return op == "narrow-commands" || op == "migrate-commands"
}

type ManagedCommandAdmission struct {
	Operation string `json:"operation"`
	Mode      string `json:"mode"`
	Hash      string `json:"hash"`
}

func (a ManagedCommandAdmission) validate() error {
	if !safefs.ValidID(a.Operation, "n") || !safefs.DigestPattern.MatchString(a.Hash) || (a.Mode != "read" && a.Mode != "all" && a.Mode != "drain") {
		return fmt.Errorf("invalid command admission")
	}
	return nil
}

type managedCommandRecord struct {
	Version           int    `json:"version"`
	Operation         string `json:"operation"`
	BeforeHash        string `json:"before_hash"`
	ActivationHash    string `json:"activation_hash"`
	QualificationHash string `json:"qualification_hash"`
	Mode              string `json:"mode"`
	Policy            string `json:"policy,omitempty"`
}

func validateCommandJournal(j managedJournal) error {
	i := j.Intent
	if i.Version != managedVersion || !safefs.ValidID(i.ID, "n") || i.Before == nil || i.Before.validate() != nil || i.After.validate() != nil || managedPhase(j.Phase) == 0 || i.Supersedes != nil {
		return fmt.Errorf("invalid command lifecycle intent")
	}
	b, a := *i.Before, i.After
	if b.Mode != "delivery" || a.Mode != "delivery" || a.Commands == nil || a.Commands.Operation != i.ID {
		return fmt.Errorf("command admission requires retained delivery")
	}
	if i.Operation == "enable-commands" {
		if a.Commands.Mode == "drain" || (b.Commands != nil && b.Commands.Mode != "drain") {
			return fmt.Errorf("disable existing command admission before enabling a different mode")
		}
	} else if isManagedCommandPolicyOperation(i.Operation) {
		if a.Commands.Mode != "drain" || b.Commands == nil || b.Commands.Mode != "drain" {
			return fmt.Errorf("disable commands before narrowing")
		}
	} else if i.Operation != "disable-commands" || a.Commands.Mode != "drain" || b.Commands == nil || b.Commands.Mode == "drain" {
		return fmt.Errorf("invalid command disable transition")
	}
	want := b
	want.Commands = a.Commands
	want.CommandHistoryHash = a.CommandHistoryHash
	if protocol.Hash(want) != protocol.Hash(a) || i.BeforeUnit != i.AfterUnit {
		return fmt.Errorf("command admission changed delivery, identity or configuration")
	}
	unit, err := renderManagedUnit(a)
	if err != nil {
		return err
	}
	if unit != i.AfterUnit || managedDigest([]byte(unit)) != a.UnitHash {
		return fmt.Errorf("command lifecycle unit changed")
	}
	return nil
}

func readManagedCommandAdmission(d *safefs.Directory, m ManagedManifest, c Runtime) (record managedCommandRecord, err error) {
	if m.Commands == nil || m.Commands.validate() != nil || m.Mode != "delivery" || (c.Telegram == nil && c.Signal == nil) || c.Worker.Interaction == nil {
		return record, fmt.Errorf("managed commands require admitted provider delivery")
	}
	base := "operations/" + m.Commands.Operation + "/"
	var intent managedIntent
	if err = d.ReadJSON(base+"intent.json", &intent); err != nil {
		return
	}
	if err = validateCommandJournal(managedJournal{Intent: intent, Phase: "complete"}); err != nil {
		return
	}
	if protocol.Hash(intent.After) != protocol.Hash(m) {
		return record, fmt.Errorf("command admission installation changed")
	}
	if err = d.ReadJSON(base+"command-admission.json", &record); err != nil {
		return
	}
	if ((record.Version != 1 || record.Policy != "") && (record.Version != 2 || record.Policy == "" || intent.Operation != "narrow-commands") && (record.Version != 3 || record.Policy == "" || intent.Operation != "migrate-commands")) || (intent.Operation == "narrow-commands" && record.Version != 2) || (intent.Operation == "migrate-commands" && record.Version != 3) || record.Operation != intent.ID || record.Mode != m.Commands.Mode || record.BeforeHash != protocol.Hash(*intent.Before) || record.ActivationHash != m.DeliveryActivation.Hash || protocol.Hash(record) != m.Commands.Hash {
		return record, fmt.Errorf("command admission evidence changed")
	}
	delivery, _, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return record, err
	}
	if record.QualificationHash != protocol.Hash(delivery.Qualification) {
		return record, fmt.Errorf("command admission qualification differs from delivery")
	}
	return record, nil
}

func (i managedInstaller) prepareCommands(ctx context.Context, operation, root, manifestHash, qualificationHash, mode string, dry bool, policies ...string) (zero ManagedManifest, err error) {
	policy := ""
	if len(policies) > 1 || len(policies) == 1 && !isManagedCommandPolicyOperation(operation) {
		return zero, fmt.Errorf("policy only applies to narrowing")
	}
	if len(policies) == 1 {
		policy = policies[0]
	}
	if isManagedCommandPolicyOperation(operation) {
		if policy == "" || mode != "" || qualificationHash != "" {
			return zero, fmt.Errorf("narrowing requires explicit policy and no enable flags")
		}
		if operation == "migrate-commands" {
			var configuration interaction.Config
			if protocol.Decode([]byte(policy), &configuration, 16384) != nil || configuration.Validate() != nil {
				return zero, fmt.Errorf("migration requires a complete command configuration")
			}
		} else {
			var permissions []interaction.Permission
			if protocol.Decode([]byte(policy), &permissions, 16384) != nil || permissions == nil {
				return zero, fmt.Errorf("narrowing requires a permission array; use migrate-commands for enrollment")
			}
		}
		mode = "drain"
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) || (operation != "enable-commands" && operation != "disable-commands" && !isManagedCommandPolicyOperation(operation)) {
		return zero, fmt.Errorf("command lifecycle requires exact installation pins")
	}
	if operation == "enable-commands" && (mode != "read" && mode != "all" || !safefs.DigestPattern.MatchString(qualificationHash)) {
		return zero, fmt.Errorf("command enable requires read/all mode and exact qualification")
	}
	if operation == "disable-commands" {
		if mode != "" || qualificationHash != "" {
			return zero, fmt.Errorf("disable does not take mode or qualification")
		}
		mode = "drain"
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", !dry, false)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return zero, err
	}
	if j.Phase != "complete" {
		return zero, fmt.Errorf("resume the unfinished operation before command admission")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return zero, err
	}
	b := j.Intent.After
	if protocol.Hash(b) != manifestHash || b.Mode != "delivery" {
		return zero, fmt.Errorf("command admission requires the current delivery manifest")
	}
	if operation == "enable-commands" && b.Commands != nil && b.Commands.Mode != "drain" || operation == "disable-commands" && (b.Commands == nil || b.Commands.Mode == "drain") {
		return zero, fmt.Errorf("command transition does not match current admission")
	}
	if isManagedCommandPolicyOperation(operation) && (b.Commands == nil || b.Commands.Mode != "drain") {
		return zero, fmt.Errorf("disable commands before narrowing")
	}
	c, err := verifyManagedArtifacts(d, b)
	if err != nil {
		return zero, err
	}
	if (c.Telegram == nil && c.Signal == nil) || c.Worker.Interaction == nil {
		return zero, fmt.Errorf("managed command activation requires enrolled provider intake")
	}
	proof, _, err := readDeliveryAdmission(d, b, c)
	if err != nil {
		return zero, err
	}
	if operation == "enable-commands" {
		if qualificationHash != protocol.Hash(proof.Qualification) {
			return zero, fmt.Errorf("command qualification differs from active delivery")
		}
		if err = checkDeliveryAdmission(d, b, c, time.Now().UTC()); err != nil {
			return zero, err
		}
	}
	units, err := managedUnitDirectory(b.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if !present || managedDigest(unit) != b.UnitHash {
		return zero, fmt.Errorf("command admission unit changed")
	}
	if isManagedCommandPolicyOperation(operation) {
		if err = worker.ValidateManagedCommandPolicyFile(b.Paths.State, c.Worker.Interaction, b.CommandHistoryHash, policy); err != nil {
			return zero, err
		}
	}
	if dry {
		return b, nil
	}
	if isManagedCommandPolicyOperation(operation) {
		if err = i.probeCommandPolicy(ctx, d, b, operation == "migrate-commands"); err != nil {
			return zero, err
		}
	}
	if err = i.probeDelivery(ctx, d, b); err != nil {
		return zero, err
	}
	if err = i.inspectDelivery(ctx, b, false); err != nil {
		return zero, err
	}
	if err = limitManagedHistoryFor(d, operation == "disable-commands"); err != nil {
		return zero, err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	record := managedCommandRecord{Version: 1, Operation: id, BeforeHash: manifestHash, ActivationHash: b.DeliveryActivation.Hash, QualificationHash: protocol.Hash(proof.Qualification), Mode: mode}
	if isManagedCommandPolicyOperation(operation) {
		record.Version, record.Policy = 2, policy
		if operation == "migrate-commands" {
			record.Version = 3
		}
	}
	a := b
	a.Commands = &ManagedCommandAdmission{id, mode, protocol.Hash(record)}
	a.CommandHistoryHash = a.Commands.Hash
	intent := managedIntent{Version: managedVersion, ID: id, Operation: operation, Before: &b, After: a, BeforeUnit: string(unit), AfterUnit: string(unit)}
	j = managedJournal{Intent: intent, Phase: "prepared"}
	if err = j.validate(); err != nil {
		return zero, err
	}
	if err = d.Mkdir("operations/" + id); err != nil {
		return zero, err
	}
	if err = d.WriteJSON("operations/"+id+"/command-admission.json", record, false); err != nil {
		return zero, err
	}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j, lock)
}

func runManagedCommandsCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use the managed installation")
	root := f.String("root", "", "Managed installation root")
	manifest := f.String("manifest-hash", "", "Current installation manifest hash")
	qualification := f.String("qualification-hash", "", "Exact active delivery qualification hash")
	configuration := f.String("configuration-file", "", "Absolute private complete command configuration for migration")
	permissions := f.String("permissions-file", "", "Absolute JSON array of retained permissions; [] revokes all")
	mode := f.String("mode", "", "Explicit read or all command admission")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("command lifecycle requires --local and exact pins")
	}
	var policies []string
	if args[0] == "migrate-commands" {
		if *permissions != "" || !managedPath(*configuration) {
			return fmt.Errorf("migration requires only an absolute configuration file")
		}
		raw, err := nativeSignalRead(*configuration, 16384, true)
		if err != nil {
			return err
		}
		var c interaction.Config
		if protocol.Decode(raw, &c, 16384) != nil || c.Validate() != nil {
			return fmt.Errorf("invalid command configuration")
		}
		raw, err = json.Marshal(c)
		if err != nil {
			return err
		}
		policies = []string{string(raw)}
	} else if *configuration != "" {
		return fmt.Errorf("configuration file only applies to migrate-commands")
	} else if args[0] == "narrow-commands" {
		if !managedPath(*permissions) {
			return fmt.Errorf("narrowing requires an absolute permissions file")
		}
		d, err := safefs.Open(filepath.Dir(*permissions))
		if err != nil {
			return err
		}
		defer d.Close()
		raw, err := safefs.ReadFile(d, filepath.Base(*permissions), 16384)
		if err != nil {
			return err
		}
		var p []interaction.Permission
		if err = protocol.Decode(raw, &p, 16384); err != nil || p == nil {
			return fmt.Errorf("permissions must be an explicit JSON array")
		}
		raw, err = json.Marshal(p)
		if err != nil {
			return err
		}
		policies = []string{string(raw)}
	} else if *permissions != "" {
		return fmt.Errorf("permissions file only applies to narrowing")
	}
	m, err := newManagedInstaller().prepareCommands(context.Background(), args[0], *root, *manifest, *qualification, *mode, ui.DryRun, policies...)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] command admission inputs validated; no service or authority changed")
		return err
	}
	return json.NewEncoder(out).Encode(m)
}

func (i managedInstaller) commandState(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime) error {
	cancelling := false
	if j.Intent.Operation == "deactivate-delivery" && j.Intent.Supersedes != nil && (j.Intent.Supersedes.Operation == "enable-commands" || j.Intent.Supersedes.Operation == "disable-commands" || isManagedCommandPolicyOperation(j.Intent.Supersedes.Operation)) {
		j.Intent = *j.Intent.Supersedes
		cancelling = true
	}
	if j.Intent.Operation != "enable-commands" && j.Intent.Operation != "disable-commands" && !isManagedCommandPolicyOperation(j.Intent.Operation) {
		return nil
	}
	m := j.Intent.After
	record, err := readManagedCommandAdmission(d, m, c)
	if err != nil {
		return err
	}
	if record.Policy != "" {
		if err = i.probeCommandPolicy(ctx, d, m, record.Version == 3); err != nil {
			return err
		}
	}
	if j.Intent.Operation == "enable-commands" && !cancelling {
		if err := checkDeliveryAdmission(d, m, c, time.Now().UTC()); err != nil {
			return err
		}
	}
	if j.Intent.Operation == "enable-commands" && c.Telegram != nil && m.RecoveryCommitHash != "" && managedPhase(j.Phase) < managedPhase("stopped") {
		if err := i.commandTelegramHandoff(ctx, d, m, c, cancelling); err != nil {
			return err
		}
	}
	s, err := openCanonicalManagedStore(m, c)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = requireDeliveryOwner(s, m, c); err != nil {
		return err
	}
	if c.Signal != nil {
		if err = s.RequireManagedSignalHandoff(ctx, *c.Signal); err != nil {
			return err
		}
	}
	if managedPhase(j.Phase) >= managedPhase("stopped") {
		return s.RequireManagedCommandHistory(m.CommandHistoryHash)
	}
	if record.Policy != "" {
		err = s.CommitManagedCommandPolicy(m.Commands.Hash, j.Intent.Before.CommandHistoryHash, record.Policy)
	} else {
		err = s.CommitManagedCommandAdmission(m.Commands.Mode, m.Commands.Hash, j.Intent.Before.CommandHistoryHash)
	}
	if err != nil {
		return err
	}
	return i.boundary("command-admission-committed")
}

func (i managedInstaller) probeCommandPolicy(ctx context.Context, d *safefs.Directory, m ManagedManifest, enrollment bool) error {
	probe := i.commandPolicyProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			capability := "__managed-command-policy-probe"
			if enrollment {
				capability = "__managed-command-enrollment-probe"
			}
			return probeManagedCapability(ctx, d, name, hash, capability)
		}
	}
	for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
		if err := probe(ctx, d, p.name, p.hash); err != nil {
			return err
		}
	}
	return nil
}
