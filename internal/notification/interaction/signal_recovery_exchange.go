package interaction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// ReconcileSignal exchanges offline relay history without provider polling or
// ACKs. The caller owns both leases. Completion does not permit intake.
func ReconcileSignal(ctx context.Context, source SignalRecoverySource, session SignalRecoverySession) (summary SignalRecoverySummary, err error) {
	if source == nil || session == nil {
		return summary, fmt.Errorf("Signal recovery dependencies missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cursor := source.Cursor()
	summary, err = session.SignalSummary(ctx, cursor.Account)
	if err == nil {
		return summary, source.RecordSummary(ctx, summary)
	}
	if !errors.Is(err, protocol.ErrRecoveryIncomplete) {
		return
	}
	ch, err := session.SignalChallenge(ctx, cursor)
	if err != nil {
		return
	}
	complete, err := source.Completed(ctx, ch)
	if err != nil {
		return
	}
	if complete {
		return summary, fmt.Errorf("gateway lost recorded Signal recovery completion")
	}
	m, err := source.Capture(ctx, ch)
	if err != nil {
		return
	}
	if err = m.Validate(); err != nil {
		return
	}
	if err = session.BeginSignal(ctx, ch, m); err != nil {
		return
	}
	var after uint64
	for {
		if err = ctx.Err(); err != nil {
			return
		}
		page, e := source.Page(ctx, ch, after)
		if e != nil {
			return summary, e
		}
		if len(page.Events) > 128 || after > m.EventCount() || uint64(len(page.Events)) > m.EventCount()-after ||
			page.Next != after+uint64(len(page.Events)) || page.Done != (page.Next == m.EventCount()) ||
			(len(page.Events) == 0 && !page.Done) {
			return summary, fmt.Errorf("Signal recovery page does not advance within sealed history")
		}
		if len(page.Events) > 0 {
			for i, event := range page.Events {
				if event.Position != m.PositionAt(after+uint64(i)) {
					return summary, fmt.Errorf("Signal page changed retained positions")
				}
			}
			if err = session.ImportSignal(ctx, ch, after, page.Events); err != nil {
				return
			}
		}
		if page.Done {
			break
		}
		after = page.Next
	}
	summary, err = session.FinishSignal(ctx, ch)
	if err != nil {
		return
	}
	err = source.RecordSummary(ctx, summary)
	return
}
