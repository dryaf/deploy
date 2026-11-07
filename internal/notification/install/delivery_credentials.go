package install

import (
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"slices"
)

// The manifest anchors every retained rotation. Deactivation, artifact updates
// and specialist recovery preserve this pin, so neither losing an old record
// nor starting a new admission lineage can silently release its exclusions.
func retainedDeliveryCredentials(d *safefs.Directory, j managedJournal, c Runtime) ([]string, error) {
	return deliveryCredentialAncestry(d, j, c)
}

// Historical snapshots start from their own manifest anchor, never from a
// directory listing that might include later rotations.
func deliveryCredentialAncestry(d *safefs.Directory, j managedJournal, c Runtime) ([]string, error) {
	m := j.Intent.After
	paths := (&managedProducerSession{runtime: c}).CredentialPaths()
	ref := m.ReportCredentialHistory
	for depth := 0; ref != nil; depth++ {
		if depth >= 128 || !safefs.ValidID(ref.Operation, "n") || !safefs.DigestPattern.MatchString(ref.Hash) {
			return nil, fmt.Errorf("invalid or excessive report credential history")
		}
		var intent managedIntent
		if err := d.ReadJSON("operations/"+ref.Operation+"/intent.json", &intent); err != nil {
			return nil, err
		}
		if intent.ID != ref.Operation || intent.After.Root != m.Root || intent.After.Installation != m.Installation || intent.After.ReportCredentialHistory == nil || *intent.After.ReportCredentialHistory != *ref || intent.DeliveryProvider == nil || protocol.Hash(intent.DeliveryProvider) != ref.Hash {
			return nil, fmt.Errorf("report credential history anchor changed")
		}
		after, err := managedRuntime(d, intent.After)
		if err != nil {
			return nil, err
		}
		pause, err := deliveryProviderPause(d, managedJournal{Intent: intent, Phase: "complete"}, after)
		if err != nil {
			return nil, err
		}
		before, err := managedRuntime(d, *pause.Intent.Before)
		if err != nil {
			return nil, err
		}
		paths = append(paths, (&managedProducerSession{runtime: before}).CredentialPaths()...)
		paths = append(paths, (&managedProducerSession{runtime: after}).CredentialPaths()...)
		ref = intent.Before.ReportCredentialHistory
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
