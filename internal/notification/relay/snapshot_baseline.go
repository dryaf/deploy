package relay

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func inboxSnapshotBudget(schema int) int64 {
	if schema == runtimeInboxSchema {
		return 33 << 20
	}
	return inboxSnapshotLimit
}
func (m InboxSnapshotManifest) historicalCount() uint64 {
	if m.Version == 3 {
		return m.HistoricalEvents
	}
	return m.HistoricalHighest
}
func (m InboxSnapshotManifest) validateLayout() error {
	if m.Highest > math.MaxInt64 || m.Acknowledged > m.Listed || m.Listed > m.Highest {
		return fmt.Errorf("invalid relay snapshot watermarks")
	}
	switch m.Schema {
	case 1, 2:
		if m.Version != 1 || m.Highest > 65536 || m.BaselineHash != "" || m.BaselinePosition != 0 || m.HistoricalHighest != 0 || m.RetainedEvents != 0 || m.HistoricalEvents != 0 {
			return fmt.Errorf("invalid legacy relay snapshot layout")
		}
	case runtimeInboxSchema:
		if (m.Version != 2 && m.Version != 3) || !safefs.DigestPattern.MatchString(m.BaselineHash) || m.BaselinePosition > m.Highest || m.BaselinePosition >= math.MaxInt64 || m.HistoricalHighest > m.BaselinePosition || m.historicalCount() > 65536 || m.historicalCount() > m.HistoricalHighest || (m.Version == 2 && m.HistoricalEvents != 0) || m.RetainedEvents > 65536 || m.RetainedEvents != m.historicalCount()+(m.Highest-m.BaselinePosition) || (m.Acknowledged != 0 && m.Acknowledged <= m.BaselinePosition) || (m.Listed != 0 && m.Listed <= m.BaselinePosition) {
			return fmt.Errorf("invalid baseline relay snapshot layout")
		}
	default:
		return fmt.Errorf("unsupported relay snapshot schema")
	}
	return nil
}
func (m InboxSnapshotManifest) validate(account string) error {
	if err := m.validateLayout(); err != nil {
		return err
	}
	if !safefs.ValidID(m.ID, "n") || m.Account != account || !protocol.ValidToken(account) || !m.Identity.Valid() || m.CreatedAt.IsZero() || m.DatabaseBytes <= 0 || m.DatabaseBytes > inboxSnapshotBudget(m.Schema) || !safefs.DigestPattern.MatchString(m.DatabaseHash) || !safefs.DigestPattern.MatchString(m.EventsDigest) {
		return fmt.Errorf("invalid relay snapshot manifest")
	}
	return nil
}

// The digest binds every retained event's actual position. A reserved gap never
// produces a fabricated event or contributes to the retained count. Digests are
// streamed so runtime startup need not retain a hash per historical event.
func baselineEventDigest(ctx context.Context, db *sql.DB, p interaction.SignalRecoveryCursor, highest uint64) (digest string, count uint64, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	floor, oldHigh := p.Handoff.Target.Position, p.Handoff.Summary.Manifest.EventCount()
	if highest < floor || highest > math.MaxInt64 {
		return "", 0, fmt.Errorf("baseline stream sequence regressed")
	}
	rows, err := db.QueryContext(ctx, "SELECT position,key,hash,body FROM events ORDER BY position")
	if err != nil {
		return
	}
	defer rows.Close()
	all, old := sha256.New(), sha256.New()
	expectedCount := oldHigh + highest - floor
	for rows.Next() {
		var pos uint64
		var key, hash, raw string
		var event interaction.SpoolEvent
		if err = rows.Scan(&pos, &key, &hash, &raw); err != nil {
			return
		}
		if err = protocol.Decode([]byte(raw), &event, 16384); err != nil {
			return
		}
		expected := p.Handoff.Summary.Manifest.PositionAt(count)
		if count >= oldHigh {
			expected = floor + 1 + (count - oldHigh)
		}
		if count >= expectedCount || pos != expected || event.Position != 0 || event.Key != key || event.Hash != hash {
			return "", count, fmt.Errorf("baseline event position or identity changed")
		}
		event.Position = pos
		if err = interaction.ValidateSignalRecoveryEvent(event, p.Handoff.Target); err != nil {
			return
		}
		hash = protocol.Hash(event)
		all.Write([]byte(hash))
		if count < oldHigh {
			old.Write([]byte(hash))
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return
	}
	if count != expectedCount || hex.EncodeToString(old.Sum(nil)) != p.Handoff.Summary.Manifest.Digest {
		return "", count, fmt.Errorf("baseline retained history incomplete or changed")
	}
	return hex.EncodeToString(all.Sum(nil)), count, nil
}
func inspectBaselineSnapshot(ctx context.Context, db *sql.DB, m *InboxSnapshotManifest, pin inboxPin) error {
	proof, err := readInboxBaseline(db, pin)
	if err != nil {
		return err
	}
	if proof == nil {
		return fmt.Errorf("baseline snapshot lost proof")
	}
	inbox := &Inbox{db: db, identity: pin.Identity, account: pin.Account, baseline: proof}
	if err = inbox.checkBaseline(db); err != nil {
		return err
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name IN ('restore_identity','restore_binding')").Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("runtime snapshot contains unconsumed restore state")
	}
	m.Version = 2
	if proof.Handoff.Summary.Manifest.BaselineHash != "" {
		m.Version = 3
		m.HistoricalEvents = proof.Handoff.Summary.Manifest.EventCount()
	}
	m.BaselineHash = pin.Baseline
	m.BaselinePosition = proof.Handoff.Target.Position
	m.HistoricalHighest = proof.Handoff.Summary.Manifest.Highest
	m.EventsDigest, m.RetainedEvents, err = baselineEventDigest(ctx, db, *proof, m.Highest)
	if err != nil {
		return err
	}
	return m.validateLayout()
}
