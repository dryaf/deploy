package interaction

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestSignalHistoryContract(t *testing.T) {
	m := SignalRecoveryManifest{Relay: SpoolCursor{Identity: protocol.Identity{Worker: "relay", Recovery: "epoch"}, Account: "account"}, Highest: 100002, Digest: protocol.Hash("history"), BaselineHash: protocol.Hash("proof"), BaselinePosition: 100000, RetainedEvents: 5, History: []SignalRange{{1, 3}, {100001, 100002}}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint64{1, 2, 3, 100001, 100002, 0} {
		if got := m.PositionAt(uint64(i)); got != want {
			t.Fatal(got, want)
		}
	}
	for _, kind := range []string{"overlap", "adjacent", "inverted", "zero", "count", "gap", "ack", "huge", "legacy", "budget"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(m)
			var bad SignalRecoveryManifest
			if err := json.Unmarshal(raw, &bad); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "overlap":
				bad.History[1].First = 3
			case "adjacent":
				bad.History[1].First = 4
			case "inverted":
				bad.History[0].First = 4
			case "zero":
				bad.History[0].First = 0
			case "count":
				bad.RetainedEvents++
			case "gap":
				bad.History[1].First++
				bad.RetainedEvents--
			case "ack":
				bad.Acknowledged = 1
				bad.Listed = 1
			case "huge":
				bad.Highest = math.MaxUint64
			case "legacy":
				bad.BaselineHash = ""
			case "budget":
				bad.History = make([]SignalRange, MaxSignalHistoryRanges+1)
			}
			if err := bad.Validate(); err == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
	// New optional fields must not alter hashes of legacy recovery evidence.
	legacy := SignalRecoveryManifest{Relay: m.Relay, Highest: 3, Digest: m.Digest}
	raw, _ := json.Marshal(legacy)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 {
		t.Fatal("legacy wire shape changed", string(raw))
	}
}
