package worker

import (
	"math"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestTelegramHandoffFractionalMonotonicDeadline(t *testing.T) {
	at := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	hash := protocol.Hash("retained-deadline-fixture")
	cursor := TelegramRecoveryCursor{Version: 1, RequestsHash: hash, NotBefore: at.Add(telegramHandoffSkew), Drain: TelegramDrainSummary{Operation: "operation", Account: "123", Identity: protocol.Identity{Worker: "worker", Recovery: "epoch"}, Pages: 1, ObservedAt: at, BootID: "boot", HistoryHash: hash, EvidenceDigest: hash}}
	for _, uptime := range []float64{0.01, 1.11, 16202.63, 1000000.63, 1000000000.63} {
		cursor.Drain.Uptime = uptime
		deadline := uptime + telegramHandoffLifetime.Seconds()
		wall := at.Add(telegramHandoffLifetime - time.Second)
		if err := cursor.fresh(wall, "boot", math.Nextafter(deadline, math.Inf(-1))); err != nil {
			t.Fatalf("uptime %.2f: handoff expired before deadline: %v", uptime, err)
		}
		for _, up := range []float64{deadline, math.Nextafter(deadline, math.Inf(1))} {
			if cursor.fresh(wall, "boot", up) == nil {
				t.Fatalf("uptime %.2f: expired monotonic deadline accepted", uptime)
			}
		}
	}
}
