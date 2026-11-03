package worker

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
)

// A durable token bucket allows an initial burst of five and replenishes one
// admission per six seconds. Confirmations consume the same actor/account budget;
// replay of a retained inbound identity bypasses it without creating more work.
func (s *Store) commandRate(tx *sql.Tx, in interaction.Inbound) error {
	var actor, conversation int
	if err := tx.QueryRow("SELECT COALESCE(sum(provider=? AND account=? AND actor=?),0),COALESCE(sum(conversation=?),0) FROM command_admissions WHERE created>?", in.Provider, in.Account, in.Actor, in.Conversation, s.now().Add(-time.Minute).UnixNano()).Scan(&actor, &conversation); err != nil {
		return err
	}
	if actor >= 10 || conversation >= 60 {
		return interaction.RejectQuietly("command rate exceeded")
	}
	unit := int64(6 * time.Second)
	capacity := 5 * unit
	credit := capacity
	now := s.now().UnixNano()
	updated := now
	err := tx.QueryRow("SELECT credit,updated FROM command_rate WHERE provider=? AND account=? AND actor=?", in.Provider, in.Account, in.Actor).Scan(&credit, &updated)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if now < updated {
		return fmt.Errorf("clock precedes admission budget")
	}
	elapsed := now - updated
	if elapsed > capacity {
		elapsed = capacity
	}
	credit += elapsed
	if credit > capacity {
		credit = capacity
	}
	if credit < unit {
		return interaction.RejectQuietly("command burst exceeded")
	}
	_, err = tx.Exec("INSERT INTO command_rate VALUES(?,?,?,?,?) ON CONFLICT(provider,account,actor) DO UPDATE SET credit=excluded.credit,updated=excluded.updated", in.Provider, in.Account, in.Actor, credit-unit, now)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO command_admissions VALUES(?,?,?,?,?,?)", in.RequestKey(), in.Provider, in.Account, in.Actor, in.Conversation, now)
	return err
}
