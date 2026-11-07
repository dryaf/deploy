package worker

import (
	"database/sql"
	"fmt"
)

// Command enrollment reserves names and policy, never provider cursors or
// command admission. Interrupted enrollment may have its exact alias set only.
func checkBootstrapCommandEnrollment(db *sql.DB, e bootstrapEnrollment) error {
	if e.Receipt.Version != 3 {
		return nil
	}
	for _, table := range []string{"commands", "command_inbox", "command_results", "command_replies", "command_reply_attempts", "telegram_cursors", "provider_inbox", "command_rate", "signal_imports", "command_admissions", "interaction_contexts", "notification_references", "notification_deliveries", "gateway_queries", "gateway_pages"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("initial command enrollment found existing interaction history")
		}
	}
	rows, err := db.Query("SELECT alias,producer,epoch FROM command_aliases")
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var alias, producer, epoch string
		if err = rows.Scan(&alias, &producer, &epoch); err != nil {
			return err
		}
		found := false
		for _, w := range e.Config.Interaction.Workers {
			if w.Alias == alias && w.Producer.ID == producer && w.Producer.Epoch == epoch {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("initial command enrollment found foreign alias authority")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var anchor string
	err = db.QueryRow("SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if anchor == "" && count != 0 || anchor != "" && (anchor != e.digest() || count != len(e.Config.Interaction.Workers)) {
		return fmt.Errorf("command alias enrollment differs from its transaction")
	}
	return nil
}

// BootstrapEnrollmentCommandsRequired verifies the retained receipt and SQL
// anchor, including missing-receipt detection, before helper compatibility checks.
func BootstrapEnrollmentCommandsRequired(state string) (bool, error) {
	e, err := retainedBootstrapEnrollment(state)
	return e != nil && e.Receipt.Version == 3, err
}

func requireBootstrapCommandAliases(db *sql.DB, e bootstrapEnrollment) error {
	if e.Receipt.Version != 3 || !e.Complete {
		return nil
	}
	for _, w := range e.Config.Interaction.Workers {
		var producer, epoch string
		if err := db.QueryRow("SELECT producer,epoch FROM command_aliases WHERE alias=?", w.Alias).Scan(&producer, &epoch); err != nil || producer != w.Producer.ID || epoch != w.Producer.Epoch {
			return fmt.Errorf("initial command alias authority missing or changed")
		}
	}
	return nil
}
