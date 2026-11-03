package interaction

import (
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalRange describes retained positions, inclusively. Omitted positions are
// unavailable evidence, never implicit acknowledgements or synthesized inputs.
type SignalRange struct{ First, Last uint64 }

const MaxSignalHistoryRanges = 32

func (m SignalRecoveryManifest) Equal(other SignalRecoveryManifest) bool {
	return protocol.Hash(m) == protocol.Hash(other)
}

func (m SignalRecoveryManifest) EventCount() uint64 {
	if m.BaselineHash == "" {
		return m.Highest
	}
	return m.RetainedEvents
}

func (m SignalRecoveryManifest) RetainsPosition(position uint64) bool {
	if position == 0 || position > m.Highest {
		return false
	}
	if m.BaselineHash == "" {
		return true
	}
	for _, r := range m.History {
		if position >= r.First && position <= r.Last {
			return true
		}
	}
	return false
}

// PositionAt maps a zero-based recovery ordinal to an actual stream position.
// Zero means out of range. Callers validate the manifest before using it.
func (m SignalRecoveryManifest) PositionAt(index uint64) uint64 {
	if index >= m.EventCount() {
		return 0
	}
	if m.BaselineHash == "" {
		return index + 1
	}
	for _, r := range m.History {
		n := r.Last - r.First + 1
		if index < n {
			return r.First + index
		}
		index -= n
	}
	return 0
}

func (m SignalRecoveryManifest) validateHistory() error {
	if m.BaselineHash == "" {
		if m.Highest > 65536 || len(m.History) != 0 || m.RetainedEvents != 0 || m.BaselinePosition != 0 {
			return fmt.Errorf("invalid legacy Signal history")
		}
		return nil
	}
	if !safefs.DigestPattern.MatchString(m.BaselineHash) || m.BaselinePosition > m.Highest || m.RetainedEvents > 65536 || len(m.History) > MaxSignalHistoryRanges ||
		(m.Acknowledged != 0 && m.Acknowledged <= m.BaselinePosition) || (m.Listed != 0 && m.Listed <= m.BaselinePosition) {
		return fmt.Errorf("invalid Signal baseline history")
	}
	var previous, count uint64
	for i, r := range m.History {
		if r.First == 0 || r.Last < r.First || r.Last > m.Highest || (i > 0 && r.First <= previous+1) || r.Last-r.First+1 > 65536-count {
			return fmt.Errorf("Signal history ranges overlap, are unbounded or noncanonical")
		}
		count += r.Last - r.First + 1
		previous = r.Last
	}
	if count != m.RetainedEvents {
		return fmt.Errorf("Signal retained count differs from ranges")
	}
	if m.Highest > m.BaselinePosition {
		if len(m.History) == 0 || previous != m.Highest || m.History[len(m.History)-1].First > m.BaselinePosition+1 {
			return fmt.Errorf("Signal post-baseline history contains a gap")
		}
	}
	return nil
}

// RecoveredHistory carries every older range forward, then adds the contiguous
// tail above the installed baseline. Repeated recovery cannot erase old gaps.
func RecoveredHistory(previous SignalRecoveryManifest, baseline, highest uint64) ([]SignalRange, error) {
	if err := previous.Validate(); err != nil {
		return nil, err
	}
	if baseline < previous.Highest || highest < baseline {
		return nil, fmt.Errorf("Signal baseline regressed")
	}
	history := append([]SignalRange(nil), previous.History...)
	if previous.BaselineHash == "" && previous.Highest > 0 {
		history = []SignalRange{{1, previous.Highest}}
	}
	if highest > baseline {
		if len(history) > 0 && history[len(history)-1].Last == baseline {
			history[len(history)-1].Last = highest
		} else {
			history = append(history, SignalRange{baseline + 1, highest})
		}
	}
	if len(history) > MaxSignalHistoryRanges {
		return nil, fmt.Errorf("Signal retained range budget exhausted")
	}
	return history, nil
}
