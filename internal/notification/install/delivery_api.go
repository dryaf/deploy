package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// DeliveryAdmissionRequest is private coordinator state, not desired YAML.
// Freezing the exact acknowledged qualification prevents a retry from selecting
// a different report or silently renewing evidence.
type DeliveryAdmissionRequest struct {
	Root              string `json:"root"`
	ManifestHash      string `json:"manifest_hash"`
	QualificationHash string `json:"qualification_hash"`
}

func (r DeliveryAdmissionRequest) validate() error {
	if !managedPath(r.Root) || !safefs.DigestPattern.MatchString(r.ManifestHash) || !safefs.DigestPattern.MatchString(r.QualificationHash) {
		return fmt.Errorf("delivery admission requires exact installation and qualification pins")
	}
	return nil
}

// ReviewDeliveryAdmission is read-only. It returns ready=false for absent,
// incomplete or stale evidence, and never fills human observations itself.
func ReviewDeliveryAdmission(ctx context.Context, root, manifestHash string) (out DeliveryAdmissionRequest, ready bool, err error) {
	if err = ctx.Err(); err != nil {
		return out, false, err
	}
	if !managedPath(root) || !safefs.DigestPattern.MatchString(manifestHash) {
		return out, false, fmt.Errorf("delivery review requires exact installation pins")
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
	m, c, err := qualificationInstallation(d, root)
	if err != nil {
		return out, false, err
	}
	if m.Mode != "verification" || protocol.Hash(m) != manifestHash {
		return out, false, fmt.Errorf("delivery review installation changed")
	}
	if len(c.Worker.Producers) == 0 {
		return out, false, nil
	}
	h, err := readQualificationHistory(d, m.Installation)
	if err != nil {
		return out, false, err
	}
	if h.Pending || len(h.Records) == 0 {
		return out, false, nil
	}
	record := h.Records[len(h.Records)-1]
	_, priorQualification, err := qualificationPause(d, m, c)
	if err != nil {
		return out, false, err
	}
	if protocol.Hash(record) == priorQualification {
		return out, false, nil
	}
	if checkDeliveryQualification(d, m, c, record, time.Now().UTC()) != nil {
		return out, false, nil
	}
	if err = deliveryPolicy(m, c); err != nil {
		return out, false, err
	}
	return DeliveryAdmissionRequest{root, manifestHash, protocol.Hash(record)}, true, ctx.Err()
}

// EnsureDeliveryAdmission activates only the gateway's qualified delivery
// service. Specialist producer generations remain inactive until their owners
// complete their separate report/ownership transition.
func EnsureDeliveryAdmission(ctx context.Context, r DeliveryAdmissionRequest) (ManagedManifest, error) {
	if err := r.validate(); err != nil {
		return ManagedManifest{}, err
	}
	return newManagedInstaller().prepareDelivery(ctx, "activate-delivery", r.Root, r.ManifestHash, r.QualificationHash, false)
}

func InspectDeliveryAdmission(ctx context.Context, r DeliveryAdmissionRequest) (ManagedManifest, bool, error) {
	return newManagedInstaller().inspectDeliveryAdmission(ctx, r)
}

func (i managedInstaller) inspectDeliveryAdmission(ctx context.Context, r DeliveryAdmissionRequest) (out ManagedManifest, complete bool, err error) {
	if err = r.validate(); err != nil {
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
	if j.Intent.After.Root != r.Root {
		return out, false, fmt.Errorf("delivery installation root changed")
	}
	if j.Phase == "complete" && j.Intent.After.Mode == "verification" && protocol.Hash(j.Intent.After) == r.ManifestHash {
		if err = validateManagedPointer(d, j, true); err != nil {
			return out, false, err
		}
		return j.Intent.After, false, nil
	}
	if j.Intent.Operation != "activate-delivery" || j.Intent.Before == nil || protocol.Hash(*j.Intent.Before) != r.ManifestHash {
		return out, false, fmt.Errorf("delivery admission belongs to another transition")
	}
	if err = validateManagedPointer(d, j, managedPhase(j.Phase) >= managedPhase("published")); err != nil {
		return out, false, err
	}
	c, err := verifyManagedArtifacts(d, j.Intent.After)
	if err != nil {
		return out, false, err
	}
	proof, _, err := readDeliveryAdmission(d, j.Intent.After, c)
	if err != nil {
		return out, false, err
	}
	if protocol.Hash(proof.Qualification) != r.QualificationHash {
		return out, false, fmt.Errorf("delivery admission qualification changed")
	}
	if j.Phase != "complete" {
		return j.Intent.After, false, nil
	}
	if err = checkDeliveryAdmission(d, j.Intent.After, c, time.Now().UTC()); err != nil {
		return out, false, err
	}
	// Completed resume performs canonical service and artifact checks without
	// starting or stopping services; use the same domain completion verifier.
	out, err = i.resumeLocked(ctx, d, j, lock)
	return out, err == nil, err
}
