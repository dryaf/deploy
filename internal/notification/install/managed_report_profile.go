package install

import (
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// ManagedReportProfile exposes only the credential path exclusions needed by a
// specialist's report service. It grants no producer or delivery authority.
type ManagedReportProfile struct {
	ManifestHash  string            `json:"manifest_hash"`
	Identity      protocol.Identity `json:"identity"`
	UnitDirectory string            `json:"unit_directory"`
	Credentials   []string          `json:"credentials"`
}

// InspectManagedReportProfile reads a complete installation under its operation
// and installation leases, without stopping services or opening the live DB.
func InspectManagedReportProfile(root string, identity protocol.Identity, producer protocol.Producer, route string) (out ManagedReportProfile, err error) {
	if !managedPath(root) || !identity.Valid() || !producer.Valid() || !protocol.ValidToken(route) {
		return out, fmt.Errorf("invalid report profile pins")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		return out, err
	}
	defer d.Close()
	op, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return out, err
	}
	defer op.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		return out, err
	}
	defer lease.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return out, err
	}
	m := j.Intent.After
	if j.Phase != "complete" || m.Root != root || m.Identity != identity || m.DeliveryOwnerHash == "" || m.Mode == "uninstalled" {
		return out, fmt.Errorf("report gateway installation changed or unfinished")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return out, err
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return out, err
	}
	enrolled := false
	for _, p := range c.Worker.Producers {
		if p.Producer == producer && slices.Contains(p.Routes, route) {
			enrolled = true
		}
	}
	if !enrolled {
		return out, fmt.Errorf("report producer/route no longer enrolled")
	}
	units, err := managedUnitDirectory(m.UnitDirectory)
	if err != nil {
		return out, err
	}
	defer units.Close()
	b, present, err := managedUnitBytes(units)
	if err != nil {
		return out, err
	}
	if !present || managedDigest(b) != m.UnitHash {
		return out, fmt.Errorf("report gateway unit changed")
	}
	paths, err := retainedDeliveryCredentials(d, j, c)
	if err != nil {
		return out, err
	}
	return ManagedReportProfile{ManifestHash: protocol.Hash(m), Identity: m.Identity, UnitDirectory: m.UnitDirectory, Credentials: paths}, nil
}
