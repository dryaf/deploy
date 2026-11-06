package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type ManagedDeliveryActivation struct {
	Operation string `json:"operation"`
	Hash      string `json:"hash"`
}

type deliveryAdmission struct {
	Version       int                         `json:"version"`
	Operation     string                      `json:"operation"`
	BeforeHash    string                      `json:"before_hash"`
	Qualification QualificationRecord         `json:"qualification"`
	Owner         worker.ManagedDeliveryOwner `json:"owner"`
}

func validateDeliveryJournal(j managedJournal) error {
	i := j.Intent
	if i.Version != managedVersion || !safefs.ValidID(i.ID, "n") || i.Before == nil || i.Before.validate() != nil || i.After.validate() != nil || managedPhase(j.Phase) == 0 {
		return fmt.Errorf("invalid delivery lifecycle intent")
	}
	b := *i.Before
	want := b
	if i.Operation == "activate-delivery" {
		if b.Mode != "verification" || i.After.Mode != "delivery" || i.Supersedes != nil || i.After.DeliveryActivation.Operation != i.ID {
			return fmt.Errorf("invalid delivery activation transition")
		}
		want.Mode = "delivery"
		want.DeliveryOwnerHash = i.After.DeliveryOwnerHash
		want.DeliveryActivation = i.After.DeliveryActivation
		if b.DeliveryOwnerHash == "" && b.RecoveryCommitHash != "" {
			r := i.After.DeliveryRecovery
			if r == nil || r.Owner.Identity != b.Identity {
				return fmt.Errorf("initial recovered enrollment must name current identity")
			}
			want.DeliveryRecovery = r
		}
		if b.DeliveryOwnerHash != "" && b.DeliveryOwnerHash != want.DeliveryOwnerHash {
			return fmt.Errorf("delivery activation changed retained owner")
		}
	} else {
		if b.Mode != "delivery" || i.After.Mode != "verification" {
			return fmt.Errorf("invalid delivery deactivation transition")
		}
		want.Mode = "verification"
		want.DeliveryActivation = nil
		want.Commands = nil
	}
	want.UnitHash = i.After.UnitHash
	if protocol.Hash(want) != protocol.Hash(i.After) {
		return fmt.Errorf("delivery lifecycle changed revision/configuration/identity")
	}
	old, err := renderManagedUnit(b)
	if err != nil {
		return err
	}
	if i.Supersedes != nil {
		p := i.Supersedes
		if i.Operation != "deactivate-delivery" || (p.Operation != "activate-delivery" && p.Operation != "enable-commands" && p.Operation != "disable-commands" && !isManagedCommandPolicyOperation(p.Operation)) || p.Supersedes != nil || protocol.Hash(p.After) != protocol.Hash(b) || (i.BeforeUnit != p.BeforeUnit && i.BeforeUnit != p.AfterUnit) {
			return fmt.Errorf("invalid activation cancellation")
		}
		if err := (managedJournal{Intent: *p, Phase: "prepared"}).validate(); err != nil {
			return err
		}
	} else if old != i.BeforeUnit {
		return fmt.Errorf("delivery prior unit changed")
	}
	after, err := renderManagedUnit(i.After)
	if err != nil {
		return err
	}
	if after != i.AfterUnit || managedDigest([]byte(after)) != i.After.UnitHash {
		return fmt.Errorf("delivery unit changed")
	}
	return nil
}

func deliveryOwner(m ManagedManifest, c Runtime) worker.ManagedDeliveryOwner {
	if m.DeliveryRecovery != nil {
		return m.DeliveryRecovery.Owner
	}
	return worker.ManagedDeliveryOwner{Version: 1, Root: m.Root, Installation: m.Installation, Identity: m.Identity, ConfigHash: protocol.Hash(c.Worker)}
}

func requireDeliveryOwner(s *worker.Store, m ManagedManifest, c Runtime) error {
	if m.DeliveryRecovery != nil {
		return s.RequireManagedDeliveryRecovery(*m.DeliveryRecovery, m.DeliveryOwnerHash)
	}
	return s.RequireManagedDeliveryOwner(deliveryOwner(m, c), m.DeliveryOwnerHash)
}

// Activation supports fixed-destination Telegram, including explicitly adopted
// recovery of an enrolled owner. Producer generations require separate activation.
func deliveryPolicy(m ManagedManifest, c Runtime) error {
	return deliveryPolicyWithSuccessor(m, c, nil)
}
func deliveryPolicyWithSuccessor(m ManagedManifest, c Runtime, successor *SignalVerificationReference) error {
	for _, path := range []string{m.Root, m.UnitDirectory, m.Paths.State} {
		if strings.ContainsAny(path, " \t") {
			return fmt.Errorf("delivery service currently requires paths without whitespace")
		}
	}
	if len(c.Providers) == 0 && c.LEDSocket == "" {
		return fmt.Errorf("delivery activation requires qualified output")
	}
	if m.SignalVerification != nil || m.SignalProfiles != nil || c.Signal != nil {
		if _, err := signalDeliveryServicePolicy(m, c, successor); err != nil {
			return err
		}
	}
	if m.Mode == "delivery" && m.RecoveryCommitHash != "" && c.Telegram != nil && m.TelegramRenewalID == "" {
		return fmt.Errorf("recovered interactive Telegram delivery requires a retained renewal")
	}
	if deliveryOwner(m, c).ConfigHash != protocol.Hash(c.Worker) {
		return fmt.Errorf("delivery ownership configuration changed")
	}
	for _, p := range c.Providers {
		if p.Kind != "telegram" && !(p.Kind == "signal" && c.Signal != nil) {
			return fmt.Errorf("unsupported delivery activation provider")
		}
	}
	return nil
}

func readDeliveryAdmission(d *safefs.Directory, m ManagedManifest, c Runtime) (out deliveryAdmission, before ManagedManifest, err error) {
	if m.DeliveryActivation == nil || m.Mode != "delivery" {
		return out, before, fmt.Errorf("no managed delivery activation")
	}
	base := "operations/" + m.DeliveryActivation.Operation + "/"
	var intent managedIntent
	if err = d.ReadJSON(base+"intent.json", &intent); err != nil {
		return
	}
	if err = (managedJournal{Intent: intent, Phase: "complete"}).validate(); err != nil {
		return
	}
	activationManifest := m
	activationManifest.Commands = nil
	activationManifest.CommandHistoryHash = intent.After.CommandHistoryHash
	if intent.Operation != "activate-delivery" || intent.Before == nil || protocol.Hash(intent.After) != protocol.Hash(activationManifest) {
		return out, before, fmt.Errorf("delivery activation intent changed")
	}
	before = *intent.Before
	if err = d.ReadJSON(base+"delivery-admission.json", &out); err != nil {
		return
	}
	if out.Version != 1 || out.Operation != intent.ID || protocol.Hash(out) != m.DeliveryActivation.Hash || out.BeforeHash != protocol.Hash(before) || out.Owner != deliveryOwner(m, c) || protocol.Hash(out.Owner) != m.DeliveryOwnerHash || out.Qualification.Report.validate() != nil || protocol.Hash(out.Qualification.Report.Binding.Manifest) != protocol.Hash(before) {
		return out, before, fmt.Errorf("delivery admission evidence changed")
	}
	err = deliveryPolicy(m, c)
	return
}

func checkDeliveryQualification(d *safefs.Directory, before ManagedManifest, c Runtime, record QualificationRecord, now time.Time) error {
	h, err := readQualificationHistory(d, before.Installation)
	if err != nil {
		return err
	}
	if h.Pending || len(h.Records) == 0 || h.Head.Hash != protocol.Hash(record) || protocol.Hash(h.Records[len(h.Records)-1]) != protocol.Hash(record) {
		return fmt.Errorf("delivery requires the exact latest acknowledged qualification")
	}
	binding, err := qualificationBinding(before, c)
	if err != nil {
		return fmt.Errorf("delivery qualification inputs unavailable")
	}
	if record.Report.matches(binding, c, now) != nil || now.Before(record.RecordedAt) || !record.Report.evidenceComplete(c) {
		return fmt.Errorf("delivery qualification is incomplete, changed or expired")
	}
	for _, p := range c.Providers {
		if p.Kind == "signal" {
			continue
		}
		if err := provider.CheckTelegramProfile(p); err != nil {
			return fmt.Errorf("delivery Telegram identity or credentials changed")
		}
	}
	if c.Signal != nil {
		service, err := signalDeliveryService(before, c)
		if err != nil {
			return err
		}
		if record.Report.Environment.SignalCLISHA256 != service.Spec.Runtime.CLISHA256 {
			return fmt.Errorf("Signal qualification does not bind the admitted native CLI")
		}
	}
	if c.Telegram != nil {
		if err := provider.CheckTelegramAccount(*c.Telegram); err != nil {
			return err
		}
	}
	return nil
}

func checkDeliveryAdmission(d *safefs.Directory, m ManagedManifest, c Runtime, now time.Time) error {
	proof, before, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return err
	}
	return checkDeliveryQualification(d, before, c, proof.Qualification, now)
}

func (i managedInstaller) probeDelivery(ctx context.Context, d *safefs.Directory, m ManagedManifest) error {
	if m.DeliveryOwnerHash == "" {
		return nil
	}
	c, err := managedRuntime(d, m)
	if err != nil {
		return err
	}
	if c.Telegram != nil && m.RecoveryCommitHash != "" {
		probe := i.telegramHandoffProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-telegram-handoff-probe")
			}
		}
		for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
			if err := probe(ctx, d, p.name, p.hash); err != nil {
				return err
			}
		}
	}
	if c.Worker.Interaction != nil {
		probe := i.commandProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-commands-probe")
			}
		}
		for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
			if err := probe(ctx, d, p.name, p.hash); err != nil {
				return err
			}
		}
	}
	if c.LEDSocket != "" {
		probe := i.ledDeliveryProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-led-delivery-probe")
			}
		}
		for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
			if err := probe(ctx, d, p.name, p.hash); err != nil {
				return err
			}
		}
	}
	if m.DeliveryRecovery != nil {
		if err := i.probeCommandRecovery(ctx, d, m); err != nil {
			return err
		}
		probe := i.deliveryRecoveryProbe
		if probe == nil {
			probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
				return probeManagedCapability(ctx, d, name, hash, "__managed-delivery-recovery-probe")
			}
		}
		for _, p := range []struct{ name, hash string }{{"releases/" + m.Revision + "/deploy", m.BinaryHash}, {managedHelperName(m), m.HelperHash}} {
			if err := probe(ctx, d, p.name, p.hash); err != nil {
				return err
			}
		}
	}
	probe := i.deliveryProbe
	if probe == nil {
		probe = func(ctx context.Context, d *safefs.Directory, name, hash string) error {
			return probeManagedCapability(ctx, d, name, hash, "__managed-delivery-probe")
		}
	}
	if err := probe(ctx, d, "releases/"+m.Revision+"/deploy", m.BinaryHash); err != nil {
		return err
	}
	return probe(ctx, d, managedHelperName(m), m.HelperHash)
}

func (i managedInstaller) inspectDelivery(ctx context.Context, m ManagedManifest, allowStale bool) error {
	if m.DeliveryOwnerHash == "" {
		return nil
	}
	if i.deliveryInspect != nil {
		return i.deliveryInspect(ctx, m, allowStale)
	}
	_, err := (userDeliveryInspector{}).inspect(ctx, m, allowStale)
	return err
}

func (i managedInstaller) deliveryState(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime) error {
	if j.Intent.Supersedes != nil && (j.Intent.Supersedes.Operation == "enable-commands" || j.Intent.Supersedes.Operation == "disable-commands" || isManagedCommandPolicyOperation(j.Intent.Supersedes.Operation)) {
		return nil
	}
	if j.Intent.Operation != "activate-delivery" && !(j.Intent.Operation == "deactivate-delivery" && j.Intent.Supersedes != nil) {
		return nil
	}
	base := j.Intent.Before
	if j.Intent.Supersedes != nil {
		base = j.Intent.Supersedes.Before
	}
	if j.Intent.Operation == "activate-delivery" {
		if err := checkDeliveryAdmission(d, j.Intent.After, c, time.Now().UTC()); err != nil {
			return err
		}
	}
	if managedPhase(j.Phase) >= managedPhase("stopped") {
		s, err := openManagedStore(j.Intent.After, c)
		if err != nil {
			return err
		}
		return s.Close()
	}
	s, err := openManagedStore(*base, c)
	if err != nil {
		return err
	}
	defer s.Close()
	if j.Intent.After.DeliveryRecovery != nil {
		if base.DeliveryOwnerHash == "" {
			err = s.EnrollManagedDeliveryRecovery(*j.Intent.After.DeliveryRecovery)
		} else {
			err = requireDeliveryOwner(s, j.Intent.After, c)
		}
	} else {
		err = s.EnrollManagedDelivery(deliveryOwner(j.Intent.After, c))
	}
	if err != nil {
		return err
	}
	return i.boundary("delivery-enrolled")
}

func (i managedInstaller) prepareDelivery(ctx context.Context, operation, root, manifestHash, qualificationHash string, dry bool) (ManagedManifest, error) {
	var zero ManagedManifest
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) || operation != "activate-delivery" && operation != "deactivate-delivery" || operation == "activate-delivery" && !safefs.DigestPattern.MatchString(qualificationHash) {
		return zero, fmt.Errorf("delivery lifecycle requires exact manifest/qualification pins")
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
	if j.Intent.After.Root != root {
		return zero, fmt.Errorf("delivery installation root moved")
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return zero, err
	}
	before := j.Intent.After
	// Exact request retries use the frozen intent and never choose a new report.
	if j.Intent.Operation == operation && j.Intent.Before != nil && protocol.Hash(*j.Intent.Before) == manifestHash {
		if operation == "activate-delivery" {
			c, e := verifyManagedArtifacts(d, before)
			if e != nil {
				return zero, e
			}
			proof, _, e := readDeliveryAdmission(d, before, c)
			if e != nil {
				return zero, e
			}
			if protocol.Hash(proof.Qualification) != qualificationHash {
				return zero, fmt.Errorf("delivery retry changed qualification")
			}
			if dry {
				if e := checkDeliveryAdmission(d, before, c, time.Now().UTC()); e != nil {
					return zero, e
				}
			}
		}
		if dry {
			return before, nil
		}
		return i.resumeLocked(ctx, d, j, lock)
	}
	var supersedes *managedIntent
	if j.Phase != "complete" {
		if operation != "deactivate-delivery" || (j.Intent.Operation != "activate-delivery" && j.Intent.Operation != "enable-commands" && j.Intent.Operation != "disable-commands" && !isManagedCommandPolicyOperation(j.Intent.Operation)) || j.Intent.Supersedes != nil {
			return zero, fmt.Errorf("unfinished lifecycle operation; use resume")
		}
		p := j.Intent
		supersedes = &p
	}
	if protocol.Hash(before) != manifestHash {
		return zero, fmt.Errorf("delivery request manifest changed; inspect current operation")
	}
	if operation == "activate-delivery" && before.Mode != "verification" || operation == "deactivate-delivery" && before.Mode != "delivery" {
		return zero, fmt.Errorf("delivery request is not valid for current mode")
	}
	c, err := verifyManagedArtifacts(d, before)
	if err != nil {
		return zero, err
	}
	if operation == "activate-delivery" {
		if err = refuseManagedRecoveryPreparation(d, before); err != nil {
			return zero, err
		}
	}
	if err = deliveryPolicy(before, c); err != nil {
		return zero, err
	}
	units, err := managedUnitDirectory(before.UnitDirectory)
	if err != nil {
		return zero, err
	}
	defer units.Close()
	unit, present, err := managedUnitBytes(units)
	if err != nil {
		return zero, err
	}
	if !present || managedDigest(unit) != before.UnitHash && !(supersedes != nil && string(unit) == supersedes.BeforeUnit) {
		return zero, fmt.Errorf("delivery unit changed")
	}
	var record QualificationRecord
	if operation == "activate-delivery" {
		h, e := readQualificationHistory(d, before.Installation)
		if e != nil {
			return zero, e
		}
		if h.Pending || len(h.Records) == 0 || h.Head.Hash != qualificationHash {
			return zero, fmt.Errorf("delivery requires exact latest qualification hash")
		}
		record = h.Records[len(h.Records)-1]
		if err = checkDeliveryQualification(d, before, c, record, time.Now().UTC()); err != nil {
			return zero, err
		}
	}
	if dry {
		return before, nil
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return zero, err
	}
	after := before
	var proof deliveryAdmission
	if operation == "activate-delivery" {
		proof = deliveryAdmission{Version: 1, Operation: id, BeforeHash: manifestHash, Qualification: record, Owner: deliveryOwner(before, c)}
		after.Mode = "delivery"
		after.DeliveryOwnerHash = protocol.Hash(proof.Owner)
		after.DeliveryActivation = &ManagedDeliveryActivation{Operation: id, Hash: protocol.Hash(proof)}
		if before.RecoveryCommitHash != "" && before.DeliveryOwnerHash == "" {
			state, e := privateManagedDir(before.Paths.State, true)
			if e != nil {
				return zero, e
			}
			var adoption worker.RecoveryVerification
			e = state.ReadJSON("recovery-verification-complete.json", &adoption)
			state.Close()
			if e != nil {
				return zero, e
			}
			if adoption.Version != 1 || !adoption.VerificationOnly || adoption.After != before.Identity || adoption.CommitHash != before.RecoveryCommitHash || adoption.ConfigHash != protocol.Hash(c.Worker) {
				return zero, fmt.Errorf("recovered delivery adoption binding changed")
			}
			after.DeliveryRecovery = &worker.ManagedDeliveryRecovery{Version: 1, Owner: proof.Owner, Before: adoption.Before, After: adoption.After, CommitHash: adoption.CommitHash}
		}
	} else {
		after.Mode = "verification"
		after.DeliveryActivation = nil
		after.Commands = nil
	}
	if err = deliveryPolicy(after, c); err != nil {
		return zero, err
	}
	body, err := renderManagedUnit(after)
	if err != nil {
		return zero, err
	}
	after.UnitHash = managedDigest([]byte(body))
	if err = i.probeDelivery(ctx, d, after); err != nil {
		return zero, err
	}
	inspection := before
	if operation == "activate-delivery" {
		inspection.DeliveryOwnerHash = after.DeliveryOwnerHash
	}
	if err = i.inspectDelivery(ctx, inspection, operation == "deactivate-delivery"); err != nil {
		return zero, err
	}
	if err = limitManagedHistoryFor(d, operation == "deactivate-delivery"); err != nil {
		return zero, err
	}
	intent := managedIntent{Version: managedVersion, ID: id, Operation: operation, Before: &before, After: after, BeforeUnit: string(unit), AfterUnit: body, Supersedes: supersedes}
	if err = (managedJournal{Intent: intent, Phase: "prepared"}).validate(); err != nil {
		return zero, err
	}
	if operation == "activate-delivery" {
		if err = d.Mkdir("operations/" + id); err != nil {
			return zero, err
		}
		if err = d.WriteJSON("operations/"+id+"/delivery-admission.json", proof, false); err != nil {
			return zero, err
		}
	}
	j = managedJournal{Intent: intent, Phase: "prepared"}
	if err = writeManagedIntent(d, j); err != nil {
		return zero, err
	}
	if err = i.boundary("prepared"); err != nil {
		return zero, err
	}
	return i.resumeLocked(ctx, d, j, lock)
}
