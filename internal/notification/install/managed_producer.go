package install

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// ManagedProducerRequest pins one producer, without changing any other gate or
// the installation revision. Revocation deliberately needs no live credentials.
type ManagedProducerRequest struct {
	Root         string            `json:"root"`
	ManifestHash string            `json:"manifest_hash"`
	Producer     protocol.Producer `json:"producer"`
	Generation   string            `json:"generation"`
	Route        string            `json:"route"`
	Revoke       bool              `json:"revoke,omitempty"`
}

func (r ManagedProducerRequest) Validate() error {
	if !managedPath(r.Root) || !safefs.DigestPattern.MatchString(r.ManifestHash) || !r.Producer.Valid() || !protocol.ValidToken(r.Generation) || !protocol.ValidToken(r.Route) {
		return fmt.Errorf("producer cutover requires exact manifest, producer, generation and route")
	}
	return nil
}

// ManagedProducerSession is valid only inside WithManagedProducer. The caller
// must durably fence the old sender before Activate and publish its new owner
// before returning. No dispatcher runs while the callback holds this session.
type ManagedProducerSession interface {
	Manifest() ManagedManifest
	CredentialPaths() []string
	Status() (protocol.OwnershipStatus, error)
	Stage() error
	Activate() error
	Revoke(context.Context) error
	Receipts(context.Context, protocol.Cursor) (worker.ReceiptPage, error)
}

type managedProducerSession struct {
	request  ManagedProducerRequest
	manifest ManagedManifest
	runtime  Runtime
	store    *worker.Store
	check    func() error
}

func (s *managedProducerSession) Manifest() ManagedManifest { return s.manifest }
func (s *managedProducerSession) CredentialPaths() []string {
	var out []string
	for _, p := range s.runtime.Providers {
		if p.ChatFile != "" {
			out = append(out, p.ChatFile)
		}
		if p.TokenFile != "" {
			out = append(out, p.TokenFile)
		}
	}
	return out
}
func (s *managedProducerSession) Status() (protocol.OwnershipStatus, error) {
	if err := s.check(); err != nil {
		return protocol.OwnershipStatus{}, err
	}
	return s.store.OwnershipStatus(s.request.Producer, s.request.Generation)
}
func (s *managedProducerSession) Stage() error {
	if s.request.Revoke {
		return fmt.Errorf("revocation cannot stage generations")
	}
	status, err := s.Status()
	if err != nil {
		return err
	}
	if status.State == "staged" || status.Active {
		return nil
	}
	return s.store.Stage(s.request.Producer, s.request.Generation)
}
func (s *managedProducerSession) Activate() error {
	if s.request.Revoke {
		return fmt.Errorf("revocation cannot activate generations")
	}
	if err := s.check(); err != nil {
		return err
	}
	return s.store.Activate(s.request.Producer, s.request.Generation)
}
func (s *managedProducerSession) Revoke(ctx context.Context) error {
	if err := s.check(); err != nil {
		return err
	}
	status, err := s.store.OwnershipStatus(s.request.Producer, s.request.Generation)
	if err != nil {
		return err
	}
	if status.State == "unknown" {
		if err := s.store.Stage(s.request.Producer, s.request.Generation); err != nil {
			return err
		}
	}
	return s.store.Revoke(ctx, s.request.Producer, s.request.Generation)
}

func (s *managedProducerSession) Receipts(ctx context.Context, cursor protocol.Cursor) (worker.ReceiptPage, error) {
	if err := s.check(); err != nil {
		return worker.ReceiptPage{}, err
	}
	if cursor.Identity != s.manifest.Identity || cursor.Producer != s.request.Producer {
		return worker.ReceiptPage{}, fmt.Errorf("receipt cursor belongs to another producer")
	}
	return s.store.Receipts(ctx, cursor, 128)
}

func WithManagedProducer(ctx context.Context, r ManagedProducerRequest, fn func(ManagedProducerSession) error) error {
	return newManagedInstaller().withManagedProducer(ctx, r, fn)
}

func (i managedInstaller) withManagedProducer(ctx context.Context, r ManagedProducerRequest, fn func(ManagedProducerSession) error) error {
	if r.Validate() != nil || fn == nil {
		return fmt.Errorf("producer cutover requires exact manifest, producer, generation and route")
	}
	d, err := privateManagedDir(r.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	operation, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return err
	}
	defer operation.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		return err
	}
	if j.Phase != "complete" || j.Intent.After.Root != r.Root || protocol.Hash(j.Intent.After) != r.ManifestHash {
		return fmt.Errorf("producer cutover manifest changed or lifecycle is unfinished")
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		return err
	}
	m := j.Intent.After
	if m.Mode != "delivery" {
		return fmt.Errorf("producer cutover requires managed delivery mode")
	}
	c, err := verifyManagedArtifacts(d, m)
	if err != nil {
		return err
	}
	if err = deliveryPolicy(m, c); err != nil {
		return err
	}
	enrolled := false
	for _, e := range c.Worker.Producers {
		if e.Producer == r.Producer && slices.Contains(e.Routes, r.Route) {
			enrolled = true
		}
	}
	if !enrolled {
		return fmt.Errorf("producer/route is not enrolled")
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
		return fmt.Errorf("managed producer unit changed")
	}
	if !r.Revoke {
		if err = checkDeliveryAdmission(d, m, c, time.Now().UTC()); err != nil {
			return err
		}
	}
	if err = i.probeDelivery(ctx, d, m); err != nil {
		return err
	}
	if err = i.inspectDelivery(ctx, m, r.Revoke); err != nil {
		return err
	}
	if _, err = inspectManagedService(ctx, i.services, m, false); err != nil {
		return err
	}
	if err = i.services.Stop(ctx); err != nil {
		return err
	}
	if err = i.boundary("producer-stopped"); err != nil {
		return err
	}
	state, err := inspectManagedService(ctx, i.services, m, false)
	if err != nil {
		return err
	}
	if state.Active != "inactive" && state.Active != "failed" {
		return fmt.Errorf("managed producer service has not stopped")
	}
	lease, err := d.Lock("installation.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	s, err := openManagedStore(m, c)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if closed {
			return fmt.Errorf("producer session is closed")
		}
		if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
			return err
		}
		for name, l := range map[string]*safefs.Lock{"operation.lock": operation, "installation.lock": lease} {
			if err := managedLeaseMatches(d, name, l); err != nil {
				return err
			}
		}
		if err := validateManagedPointer(d, j, true); err != nil {
			return err
		}
		if err := s.RequireManagedDeliveryOwner(deliveryOwner(m, c), m.DeliveryOwnerHash); err != nil {
			return err
		}
		if !r.Revoke {
			return checkDeliveryAdmission(d, m, c, time.Now().UTC())
		}
		return nil
	}
	if err = check(); err != nil {
		return err
	}
	if err = fn(&managedProducerSession{r, m, c, s, check}); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if err = s.Close(); err != nil {
		return err
	}
	closed = true
	lease.Close()
	if err = i.boundary("producer-committed"); err != nil {
		return err
	}
	// Revocation is durable even if the remaining runtime cannot restart (for
	// example, expired qualification). Never undo the tombstone on this error.
	if err = i.services.EnableStart(ctx); err != nil {
		return err
	}
	return i.services.Verify(ctx, m)
}
