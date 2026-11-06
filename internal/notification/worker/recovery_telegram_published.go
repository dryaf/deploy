package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

// PublishedTelegramHandoff is a point-in-time inspection, never a persisted
// activation grant. Receivers must remain stopped and activation must recheck.
type PublishedTelegramHandoff struct {
	RenewalHash               string                 `json:"renewal_hash,omitempty"`
	Version                   int                    `json:"version"`
	CommitHash                string                 `json:"commit_hash"`
	Cursor                    TelegramRecoveryCursor `json:"cursor"`
	CheckedAt                 time.Time              `json:"checked_at"`
	ExpiresAt                 time.Time              `json:"expires_at"`
	IntakeEnabled             bool                   `json:"intake_enabled"`
	RequiresActivationRecheck bool                   `json:"requires_activation_recheck"`
}

// VerifyPublishedTelegramHandoff checks the original handoff after publication.
// It neither polls Telegram nor renews the observation, moves cursors, modifies
// retained evidence or proves external receivers stopped. An expired handoff
// requires a separately journaled new drain before production can be activated.
func VerifyPublishedTelegramHandoff(ctx context.Context, path string, c Config, commit, account string) (PublishedTelegramHandoff, error) {
	return verifyPublishedTelegramHandoff(ctx, path, c, commit, account, time.Now, process.HostClock)
}

func verifyPublishedTelegramHandoff(ctx context.Context, path string, c Config, commit, account string, now func() time.Time, clock func() (string, float64, error)) (out PublishedTelegramHandoff, err error) {
	if now == nil || clock == nil {
		return out, fmt.Errorf("Telegram handoff clock unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s, err := openPublishedTelegramSession(ctx, path, c, commit, account)
	if err != nil {
		return out, err
	}
	defer s.close()
	return s.verifyOriginalHandoff(ctx, now, clock)
}

func (s *publishedTelegramSession) verifyOriginalHandoff(ctx context.Context, now func() time.Time, clock func() (string, float64, error)) (out PublishedTelegramHandoff, err error) {
	if err = s.requireOriginalTelegramHandoff(); err != nil {
		return out, err
	}
	if err = s.verify(ctx); err != nil {
		return out, err
	}
	if err = s.requireOriginalTelegramHandoff(); err != nil {
		return out, err
	}
	cursor := s.cursor
	boot, up, err := clock()
	if err != nil {
		return out, err
	}
	at := now().UTC()
	if err = cursor.fresh(at, boot, up); err != nil {
		return out, err
	}
	if !at.After(cursor.NotBefore) {
		return out, fmt.Errorf("Telegram recovery cutoff has not passed")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	return PublishedTelegramHandoff{Version: 1, CommitHash: s.publication.CommitHash, Cursor: cursor, CheckedAt: at, ExpiresAt: cursor.Drain.ObservedAt.Add(telegramHandoffLifetime), RequiresActivationRecheck: true}, nil
}

func readPublishedTelegramCursor(ctx context.Context, db *sql.DB, account string) (TelegramRecoveryCursor, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TelegramRecoveryCursor{}, err
	}
	defer tx.Rollback()
	cursor, err := readTelegramRecoveryCursor(tx, account)
	if err != nil {
		return TelegramRecoveryCursor{}, err
	}
	if cursor == nil {
		return TelegramRecoveryCursor{}, protocol.ErrRecoveryIncomplete
	}
	// An absent operational row must not become an implicit valid zero cursor.
	var offset int64
	if err = tx.QueryRowContext(ctx, "SELECT offset FROM telegram_cursors WHERE account=?", account).Scan(&offset); err != nil {
		return TelegramRecoveryCursor{}, err
	}
	if offset != cursor.Offset {
		return TelegramRecoveryCursor{}, fmt.Errorf("published Telegram operational cursor changed")
	}
	return *cursor, nil
}

// Once a renewal starts, the old cutoff must not be advertised as current even
// while that renewal is incomplete. A missing root index cannot hide its archive.
func (s *publishedTelegramSession) requireOriginalTelegramHandoff() error {
	if err := absentOriginalFile(s.root, s.renewalIndex()); err != nil {
		return fmt.Errorf("Telegram handoff has renewal state; inspect its exact completion: %w", err)
	}
	p, err := s.renewalParent(false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer p.Close()
	names, err := renewalNames(p, 1)
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return fmt.Errorf("Telegram renewal history lost its index")
	}
	return nil
}
