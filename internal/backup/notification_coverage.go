package backup

import (
	"database/sql"
	"fmt"
	"strings"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// Representation is not acceptance. Ignore disposed transport containers, while
// keeping source events that have not yet acquired a shared obligation visible.
const backupPendingSourcePredicate = `b.accepted IS NULL AND b.covered_by IS NULL
 AND NOT EXISTS (SELECT 1 FROM notification_obligations o WHERE o.id=CASE WHEN b.kind='delivery-catchup' THEN substr(b.id,9) ELSE b.id END AND o.disposition<>'open')
 AND (b.kind<>'batch' OR EXISTS (SELECT 1 FROM json_each(b.record,'$.members') j JOIN outbox member ON member.id=j.value LEFT JOIN notification_obligations o ON o.id=member.id WHERE member.accepted IS NULL AND (o.id IS NULL OR o.disposition='open')))`

type backupDeliveryCoverage struct {
	DailyAccepted  bool
	OutageAccepted bool
	Outstanding    int
	PendingSource  bool
	ClockUncertain bool
}

func (c backupDeliveryCoverage) complete() bool {
	return c.DailyAccepted && c.OutageAccepted && c.Outstanding == 0 && !c.PendingSource && !c.ClockUncertain
}

// Read one consistent producer view. This proves delivery coverage only; backup
// freshness, application checks and host health remain independent claims.
func readBackupDeliveryCoverage(tx *sql.Tx, period, outage string) (c backupDeliveryCoverage, err error) {
	b, err := readBackupNotificationBinding(tx)
	if err != nil {
		return c, err
	}
	if !notify.ValidToken(period) || !strings.HasPrefix(period, "daily/"+b.Producer.ID+"/") || (outage != "" && !notify.ValidToken(outage)) {
		return c, fmt.Errorf("invalid backup coverage identity")
	}
	if err = backupNotificationMeta(tx, "clock_suspect", &c.ClockUncertain); err != nil && err != sql.ErrNoRows {
		return c, err
	}
	accepted := func(id, kind string) (bool, error) {
		var found bool
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_obligations o
 JOIN notification_exports e ON e.id=o.covered_by
 JOIN notification_members m ON m.event=e.id AND m.obligation=o.id
 WHERE o.id=? AND o.kind=? AND o.disposition='satisfied' AND e.accepted=1 AND e.eligible=1)`, id, kind).Scan(&found)
		return found, err
	}
	if c.DailyAccepted, err = accepted(period, "daily"); err != nil {
		return
	}
	c.OutageAccepted = outage == ""
	if outage != "" {
		if c.OutageAccepted, err = accepted(outage, "catchup"); err != nil {
			return
		}
	}
	if err = tx.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&c.Outstanding); err != nil {
		return
	}
	err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM outbox b WHERE (" + backupPendingSourcePredicate + `)
 OR (b.kind='daily' AND b.accepted IS NULL AND b.covered_by IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM notification_obligations o WHERE o.id=b.id AND o.disposition<>'open')))`).Scan(&c.PendingSource)
	return
}
