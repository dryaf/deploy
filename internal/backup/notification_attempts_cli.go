package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/dryaf/deploy/internal/ui"
)

// The review is observational: it neither settles in-flight reservations nor
// certifies old helpers, legacy history, provider ownership or cutover readiness.
func runBackupDeliveryReviewCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup-worker delivery-review", flag.ContinueOnError)
	f.SetOutput(out)
	local := f.Bool("local", false, "Inspect existing local backup notification evidence")
	path := f.String("state-dir", "", "Existing private backup state directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !backupRecoveryPath(*path) {
		return fmt.Errorf("delivery-review requires --local --state-dir ABS and no positional arguments")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] delivery-review arguments validated; no state opened or provider contacted")
		return err
	}
	s, err := openBackupState(*path, false)
	if err != nil {
		return err
	}
	defer s.Close()
	var review legacyDeliveryReview
	err = backupTransferTx(context.Background(), s, func(tx *sql.Tx) error {
		j, err := readLegacyDeliveryJournal(tx)
		review = j.review
		return err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(review)
}
