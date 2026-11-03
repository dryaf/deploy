package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalHistoryProofFitsControlBudget(t *testing.T) {
	_, _, p := baselineInboxFixture(t, true)
	m := &p.Handoff.Summary.Manifest
	m.Highest, m.BaselinePosition = math.MaxInt64-1, math.MaxInt64-1
	m.BaselineHash = protocol.Hash("prior proof")
	m.RetainedEvents = interaction.MaxSignalHistoryRanges
	m.History = nil
	for i := uint64(0); i < interaction.MaxSignalHistoryRanges; i++ {
		m.History = append(m.History, interaction.SignalRange{First: math.MaxInt64 - 128 + i*2, Last: math.MaxInt64 - 128 + i*2})
	}
	m.Acknowledged, m.Listed = 0, 0
	s := &p.Handoff.Summary
	s.Mapped, s.Unmapped, s.Rejected = 0, m.RetainedEvents, 0
	s.GatewayHighest, s.MissingRelayFrom, s.MissingRelayThrough = 0, 0, 0
	p.Handoff.Target.Position = m.Highest
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	value := recoveryExport{Challenge: s.Challenge, Manifest: *m, Summary: s, Handoff: &p.Handoff, Cursor: &p}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded recoveryExport
	if err = protocol.Decode(raw, &decoded, 16384); err != nil {
		t.Fatalf("maximum history proof exceeds control budget (%d bytes): %v", len(raw), err)
	}
}

func baselineChallenge(t *testing.T, cursor interaction.SpoolCursor) interaction.SignalRecoveryChallenge {
	t.Helper()
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return interaction.SignalRecoveryChallenge{Operation: op, Nonce: nonce, Gateway: protocol.Identity{Worker: "gateway", Recovery: "new"}, Relay: cursor, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: "boot", Uptime: 1}
}

func baselineHandoffRelayFixture(t *testing.T, restored bool) (interaction.SignalHandoffSource, string, interaction.SignalRecoveryHandoff) {
	t.Helper()
	ctx := context.Background()
	path, artifact, m, _ := baselineSnapshotFixture(t, true, 2, true)
	var source interaction.SignalHandoffSource
	var err error
	if restored {
		_, err = PrepareInboxRestore(ctx, path, m.Account, artifact)
		if err != nil {
			t.Fatal(err)
		}
		source, err = OpenRestoredRecoveryInbox(ctx, path, m.Account)
	} else {
		source, err = OpenRecoveryInbox(path, m.Account)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	ch := baselineChallenge(t, source.Cursor())
	export, err := source.Capture(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: export, Unmapped: export.EventCount()}
	if restored {
		summary.GatewayHighest = 10
		summary.MissingRelayFrom = export.Highest + 1
		summary.MissingRelayThrough = 10
	}
	if err = source.RecordSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	p := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("seal"), Before: source.Cursor(), Target: source.Cursor()}
	p.Target.Position = max(export.Highest, summary.GatewayHighest)
	if restored {
		p.Target.Identity = export.Restore.After
	}
	return source, path, p
}

func TestSignalRepeatedBaselineRecovery(t *testing.T) {
	ctx := context.Background()
	inbox, path, previous := baselineInboxFixture(t, true)
	positions := []uint64{1, 2, 3}
	for round := 0; round < 5; round++ {
		for i := 1; i <= 2; i++ {
			key := fmt.Sprintf("round-%d-event-%d", round, i)
			if err := inbox.Enqueue(ctx, interaction.SpoolEvent{Key: key, Hash: protocol.Hash(key)}); err != nil {
				t.Fatal(err)
			}
			positions = append(positions, previous.Handoff.Target.Position+uint64(i))
		}
		if err := inbox.Close(); err != nil {
			t.Fatal(err)
		}
		artifact := filepath.Join(t.TempDir(), "snapshot")
		m, err := SnapshotInbox(ctx, path, previous.Handoff.Target.Account, artifact)
		if err != nil {
			t.Fatal(err)
		}
		if round > 0 && (m.Version != 3 || m.HistoricalEvents != uint64(len(positions)-2)) {
			t.Fatal("multiple gaps lost from snapshot", m)
		}
		var source interaction.SignalCursorSource
		restored := round%2 == 0
		if restored {
			j, e := PrepareInboxRestore(ctx, path, m.Account, artifact)
			if e != nil {
				t.Fatal(e)
			}
			verifyBaselineRestoreForTest(t, path, j)
			source, err = OpenRestoredRecoveryInbox(ctx, path, m.Account)
		} else {
			source, err = OpenRecoveryInbox(path, m.Account)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { source.Close() })
		ch := baselineChallenge(t, source.Cursor())
		export, err := source.Capture(ctx, ch)
		if err != nil {
			t.Fatal(err)
		}
		if err = export.Validate(); err != nil {
			t.Fatal(err)
		}
		if export.EventCount() != uint64(len(positions)) || export.Highest != positions[len(positions)-1] || export.BaselineHash != protocol.Hash(previous) {
			t.Fatal("export lost history", export)
		}
		// Ordinals may start inside a retained range or across a reserved gap.
		for after := uint64(0); after <= export.EventCount(); after++ {
			page, e := source.Page(ctx, ch, after)
			if e != nil {
				t.Fatal(e)
			}
			for i, event := range page.Events {
				if event.Position != positions[int(after)+i] {
					t.Fatal("gap erased", event)
				}
			}
			if page.Next != after+uint64(len(page.Events)) || page.Done != (page.Next == export.EventCount()) {
				t.Fatal(page)
			}
		}
		if _, err = source.Page(ctx, ch, export.EventCount()+1); err == nil {
			t.Fatal("out of range ordinal accepted")
		}
		summary := interaction.SignalRecoverySummary{Challenge: ch, Manifest: export, Unmapped: export.EventCount()}
		if restored {
			summary.GatewayHighest = export.Highest + 3
			summary.MissingRelayFrom = export.Highest + 1
			summary.MissingRelayThrough = summary.GatewayHighest
		}
		if err = source.RecordSummary(ctx, summary); err != nil {
			t.Fatal(err)
		}
		handoff := interaction.SignalRecoveryHandoff{Version: 1, Summary: summary, RequestsHash: protocol.Hash("seal"), Before: source.Cursor(), Target: source.Cursor()}
		handoff.Target.Position = max(export.Highest, summary.GatewayHighest)
		if restored {
			handoff.Target.Identity = export.Restore.After
		}
		proof, err := source.StageSignalCursor(ctx, handoff)
		if err != nil {
			t.Fatal(err)
		}
		again, err := source.StageSignalCursor(ctx, handoff)
		if err != nil || protocol.Hash(again) != protocol.Hash(proof) {
			t.Fatal("cursor replay changed", err)
		}
		inbox, path, previous = baselineInboxFromCandidate(t, source, proof)
		if err = source.Close(); err != nil {
			t.Fatal(err)
		}
		page, err := inbox.List(ctx, proof.Handoff.Target)
		if err != nil || len(page.Events) != 0 {
			t.Fatal("recovery replayed historical events", err)
		}
	}
}
