package install

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Replacement keeps both executable installations immutable. The old service
// must already be removed; a fresh installation acquires only verification
// authority over the same endpoint/account/inboxes.
type signalServiceReplacement struct {
	Before signalServiceBinding `json:"before"`
	After  signalServiceBinding `json:"after"`
}

func signalServicePredecessor(s SignalServiceSpec) (m SignalServiceManifest, err error) {
	if s.Previous == nil || s.validate() != nil {
		return m, fmt.Errorf("Signal replacement predecessor required")
	}
	// Bound retained-history traversal before recursive artifact auditing.
	for previous, depth := s.Previous, 0; previous != nil; depth++ {
		if depth >= 32 {
			return m, fmt.Errorf("Signal replacement history limit reached")
		}
		d, e := privateManagedDir(previous.ServiceRoot, true)
		if e != nil {
			return m, e
		}
		prior, e := readSignalService(d, "complete.json")
		d.Close()
		if e != nil {
			return m, e
		}
		if prior.ManifestHash != previous.ServiceHash || prior.Spec.Root != previous.ServiceRoot {
			return m, fmt.Errorf("Signal replacement lineage changed")
		}
		previous = prior.Spec.Previous
	}
	p := *s.Previous
	d, err := privateManagedDir(p.ServiceRoot, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	m, err = readSignalService(d, "complete.json")
	if err != nil {
		return m, err
	}
	if m.Spec.Root != p.ServiceRoot || m.ManifestHash != p.ServiceHash || m.Spec.UnitDirectory != s.UnitDirectory || m.Spec.Endpoint != s.Endpoint {
		return m, fmt.Errorf("Signal predecessor pins changed")
	}
	if err = checkSignalServiceFiles(d, m, false); err != nil {
		return m, err
	}
	if err = verifySignalServiceComplete(d, m); err != nil {
		return m, err
	}
	var removal signalServiceBinding
	if err = d.ReadJSON("remove.json", &removal); err != nil {
		return m, err
	}
	j, err := readSignalServiceJournal(d, m)
	if err != nil {
		return m, err
	}
	if removal != m.binding() || j.Phase != "removed" {
		return m, fmt.Errorf("Signal predecessor removal must be complete")
	}
	return m, nil
}

func replacementRecord(m SignalServiceManifest) signalServiceReplacement {
	return signalServiceReplacement{signalServiceBinding{m.Spec.Previous.ServiceRoot, m.Spec.Previous.ServiceHash}, m.binding()}
}

func requireReplacement(d *safefs.Directory, name string, want signalServiceReplacement) error {
	var got signalServiceReplacement
	if err := d.ReadJSON(name, &got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("Signal replacement authority changed")
	}
	return nil
}

func checkSignalServiceReplacement(d *safefs.Directory, m SignalServiceManifest) error {
	if _, err := signalServicePredecessor(m.Spec); err != nil {
		return err
	}
	want := replacementRecord(m)
	old, err := privateManagedDir(want.Before.Root, true)
	if err != nil {
		return err
	}
	defer old.Close()
	endpoint, err := privateManagedDir(m.Spec.Endpoint.Root, true)
	if err != nil {
		return err
	}
	defer endpoint.Close()
	for _, entry := range []struct {
		d    *safefs.Directory
		name string
	}{
		{old, "replacement.json"}, {old, "replacement-complete.json"},
		{endpoint, "replacement-" + m.ManifestHash + ".json"},
		{endpoint, "replacement-" + m.ManifestHash + "-complete.json"},
		{d, "enrollment-complete.json"},
	} {
		if err := requireReplacement(entry.d, entry.name, want); err != nil {
			return err
		}
	}
	return nil
}

func (i signalServiceInstaller) replaceBinding(ctx context.Context, d *safefs.Directory, m SignalServiceManifest) error {
	old, operation, err := openSignalServiceRoot(m.Spec.Previous.ServiceRoot, false)
	if err != nil {
		return err
	}
	defer old.Close()
	defer operation.Close()
	lease, err := old.Lock("installation.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	prior, err := signalServicePredecessor(m.Spec)
	if err != nil {
		return err
	}
	want := replacementRecord(m)
	if prior.binding() != want.Before {
		return fmt.Errorf("Signal predecessor changed")
	}
	// A published replacement never reconstructs missing anchors on resume.
	if err := absentServiceFile(d, "ready.json"); err != nil {
		return checkSignalServiceReplacement(d, m)
	}
	endpoint, err := privateManagedDir(m.Spec.Endpoint.Root, true)
	if err != nil {
		return err
	}
	var current signalServiceBinding
	err = endpoint.ReadJSON(signalServiceEndpointBinding, &current)
	endpoint.Close()
	if err != nil {
		return err
	}
	if current != want.Before && current != want.After {
		return fmt.Errorf("Signal endpoint replacement changed owner")
	}
	if current == want.After {
		// A changed binding proves the two independent intents were durable.
		if err = requireReplacement(old, "replacement.json", want); err != nil {
			return err
		}
		endpoint, err = privateManagedDir(m.Spec.Endpoint.Root, true)
		if err != nil {
			return err
		}
		err = requireReplacement(endpoint, "replacement-"+m.ManifestHash+".json", want)
		endpoint.Close()
		if err != nil {
			return err
		}
	}
	// This holds endpoint, account and inbox leases together. It has no send,
	// receive, link or process-launch methods.
	v, err := openSignalEndpointVerifierService(ctx, m.Spec.Endpoint.Root, m.Spec.Endpoint.ManifestHash, current)
	if err != nil {
		return err
	}
	defer v.close()
	if protocol.Hash(v.manifest) != protocol.Hash(m.Endpoint) {
		return fmt.Errorf("Signal replacement endpoint changed")
	}
	if err = runtimeJSONOnceForService(old, "replacement.json", want); err != nil {
		return err
	}
	if err = runtimeJSONOnceForService(v.root.dir, "replacement-"+m.ManifestHash+".json", want); err != nil {
		return err
	}
	if err = i.boundary("replacement-intent"); err != nil {
		return err
	}
	if current == want.Before {
		for _, entry := range []struct {
			dir  *safefs.Directory
			name string
		}{
			{d, "enrollment-complete.json"}, {old, "replacement-complete.json"},
			{v.root.dir, "replacement-" + m.ManifestHash + "-complete.json"},
		} {
			if err = absentServiceFile(entry.dir, entry.name); err != nil {
				return fmt.Errorf("Signal replacement head rolled back: %w", err)
			}
		}
		if err = v.check(ctx); err != nil {
			return err
		}
		if err = v.root.dir.WriteJSON(signalServiceEndpointBinding, want.After, true); err != nil {
			return err
		}
		v.service = want.After
	}
	if err = i.boundary("replacement-bound"); err != nil {
		return err
	}
	if err = v.check(ctx); err != nil {
		return err
	}
	if err = runtimeJSONOnceForService(old, "replacement-complete.json", want); err != nil {
		return err
	}
	if err = runtimeJSONOnceForService(v.root.dir, "replacement-"+m.ManifestHash+"-complete.json", want); err != nil {
		return err
	}
	if err = runtimeJSONOnceForService(d, "enrollment-complete.json", want); err != nil {
		return err
	}
	if err = i.boundary("replacement-complete"); err != nil {
		return err
	}
	return checkSignalServiceReplacement(d, m)
}
