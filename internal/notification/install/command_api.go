package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// CommandPolicyRequest retains an exact reviewed domain transition. It is not
// desired policy and cannot turn an outbound delivery receipt into command rights.
// The coordinator must retain it before effects; qualification is never refreshed
// implicitly on retry. Disabling and policy migration keep outbound delivery.
type CommandPolicyRequest struct {
	Root              string `json:"root"`
	ManifestHash      string `json:"manifest_hash"`
	Operation         string `json:"operation"`
	QualificationHash string `json:"qualification_hash,omitempty"`
	Mode              string `json:"mode,omitempty"`
	Policy            string `json:"policy,omitempty"`
}

func (r CommandPolicyRequest) validate() error {
	if !managedPath(r.Root) || !safefs.DigestPattern.MatchString(r.ManifestHash) {
		return fmt.Errorf("command policy requires exact installation pins")
	}
	switch r.Operation {
	case "enable-commands":
		if (r.Mode != "read" && r.Mode != "all") || !safefs.DigestPattern.MatchString(r.QualificationHash) || r.Policy != "" {
			return fmt.Errorf("command enable requires exact qualification and read/all mode")
		}
	case "disable-commands":
		if r.Mode != "" || r.QualificationHash != "" || r.Policy != "" {
			return fmt.Errorf("command disable cannot change policy or qualification")
		}
	case "narrow-commands", "migrate-commands", "restore-command-permissions":
		if r.Mode != "" || r.QualificationHash != "" || r.Policy == "" || len(r.Policy) > 16384 {
			return fmt.Errorf("command migration requires only bounded retained policy")
		}
		if r.Operation == "narrow-commands" {
			var permissions []interaction.Permission
			if protocol.Decode([]byte(r.Policy), &permissions, 16384) != nil || permissions == nil {
				return fmt.Errorf("command narrowing requires a permission array")
			}
		} else {
			var policy interaction.Config
			if protocol.Decode([]byte(r.Policy), &policy, 16384) != nil || policy.Validate() != nil {
				return fmt.Errorf("command migration requires complete validated policy")
			}
		}
	default:
		return fmt.Errorf("unsupported command policy transition")
	}
	return nil
}

// ReviewCommandAdmission only selects already acknowledged delivery qualification.
// Absence/expiry remains not-ready; no operator observation is invented here.
func ReviewCommandAdmission(ctx context.Context, root, manifestHash, mode string) (out CommandPolicyRequest, ready bool, err error) {
	if err = ctx.Err(); err != nil {
		return out, false, err
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) || (mode != "read" && mode != "all") {
		return out, false, fmt.Errorf("command review requires exact delivery installation and read/all mode")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, false, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, false, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, false, err
	}
	if j.Phase != "complete" || j.Intent.After.Root != root || protocol.Hash(j.Intent.After) != manifestHash || j.Intent.After.Mode != "delivery" {
		return out, false, fmt.Errorf("command admission requires unchanged completed delivery")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, false, err
	}
	m := j.Intent.After
	if m.Commands != nil && m.Commands.Mode != "drain" {
		return out, false, fmt.Errorf("disable existing command admission before selecting another mode")
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return out, false, err
	}
	if c.Worker.Interaction == nil || (c.Telegram == nil && c.Signal == nil) {
		return out, false, fmt.Errorf("enroll provider command intake before admission")
	}
	proof, _, err := readDeliveryAdmission(d, m, c)
	if err != nil {
		return out, false, err
	}
	if checkDeliveryAdmission(d, m, c, time.Now().UTC()) != nil {
		return out, false, nil
	}
	return CommandPolicyRequest{Root: root, ManifestHash: manifestHash, Operation: "enable-commands", QualificationHash: protocol.Hash(proof.Qualification), Mode: mode}, true, ctx.Err()
}

func EnsureCommandPolicy(ctx context.Context, r CommandPolicyRequest) (ManagedManifest, error) {
	return newManagedInstaller().ensureCommandPolicy(ctx, r)
}
func InspectCommandPolicy(ctx context.Context, r CommandPolicyRequest) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectCommandPolicy(ctx, r)
}

func matchCommandPolicyRequest(d *safefs.Directory, j managedJournal, r CommandPolicyRequest) error {
	if j.Intent.Operation != r.Operation || j.Intent.Before == nil || protocol.Hash(*j.Intent.Before) != r.ManifestHash || j.Intent.After.Root != r.Root {
		return fmt.Errorf("command policy belongs to another retained transition")
	}
	if err := validateCommandJournal(j); err != nil {
		return err
	}
	var record managedCommandRecord
	if err := d.ReadJSON("operations/"+j.Intent.ID+"/command-admission.json", &record); err != nil {
		return err
	}
	mode := r.Mode
	if r.Operation != "enable-commands" {
		mode = "drain"
	}
	if record.Operation != j.Intent.ID || record.BeforeHash != r.ManifestHash || record.Mode != mode || record.Policy != r.Policy || r.Operation == "enable-commands" && record.QualificationHash != r.QualificationHash || protocol.Hash(record) != j.Intent.After.Commands.Hash {
		return fmt.Errorf("retained command request changed")
	}
	return nil
}

func (i managedInstaller) ensureCommandPolicy(ctx context.Context, r CommandPolicyRequest) (ManagedManifest, error) {
	var zero ManagedManifest
	if err := r.validate(); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	d, err := privateManagedDir(r.Root, true)
	if err != nil {
		return zero, err
	}
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		d.Close()
		return zero, err
	}
	j, err := readManagedJournal(d)
	if err == nil && !(j.Phase == "complete" && protocol.Hash(j.Intent.After) == r.ManifestHash) {
		if err = matchCommandPolicyRequest(d, j, r); err == nil {
			zero, err = i.resumeLocked(ctx, d, j, lock)
		}
		lock.Close()
		d.Close()
		return zero, err
	}
	lock.Close()
	d.Close()
	if err != nil {
		return zero, err
	}
	if r.Policy != "" {
		return i.prepareCommands(ctx, r.Operation, r.Root, r.ManifestHash, r.QualificationHash, r.Mode, false, r.Policy)
	}
	return i.prepareCommands(ctx, r.Operation, r.Root, r.ManifestHash, r.QualificationHash, r.Mode, false)
}

func (i managedInstaller) inspectCommandPolicy(ctx context.Context, r CommandPolicyRequest) (out ManagedManifest, complete bool, err error) {
	if err = r.validate(); err != nil {
		return out, false, err
	}
	if err = ctx.Err(); err != nil {
		return out, false, err
	}
	d, err := privateManagedDir(r.Root, true)
	if err != nil {
		return out, false, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, false, err
	}
	defer lock.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, false, err
	}
	if j.Phase == "complete" && j.Intent.After.Root == r.Root && protocol.Hash(j.Intent.After) == r.ManifestHash {
		if err = validateManagedPointer(d, j, true); err != nil {
			return out, false, err
		}
		return j.Intent.After, false, nil
	}
	if err = matchCommandPolicyRequest(d, j, r); err != nil {
		return out, false, err
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return out, false, err
	}
	if j.Phase != "complete" {
		return j.Intent.After, false, nil
	}
	out, err = i.resumeLocked(ctx, d, j, lock)
	return out, err == nil, err
}
