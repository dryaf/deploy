package worker

import (
	"context"
	"database/sql"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// RecoveryHandoffSession retains one exclusive offline worker lease while a
// specialist reconciles commands and commits both delivery and command ledgers.
// It cannot publish the candidate, enable execution, or start provider intake.
type RecoveryHandoffSession interface {
	protocol.RecoveryProducerSession
	interaction.CommandRecoveryInstallSession
	RequireCommandRecoveryAbsent(context.Context, protocol.Producer) error
}

// Lack of current command enrollment is not proof that retained command history
// never existed. A notification-only source must not bypass that history.
func (r *recoveryReconciler) RequireCommandRecoveryAbsent(ctx context.Context, p protocol.Producer) error {
	if _, err := r.config.enrollment(p); err != nil {
		return err
	}
	return r.transaction(ctx, func(tx *sql.Tx) error { return requireRecoveryCommandsAbsent(tx, p) })
}

func OpenRecoveryHandoffSession(ctx context.Context, path string, c Config) (RecoveryHandoffSession, error) {
	s, err := OpenRecoveryProducerSession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	r := s.(*recoveryReconciler)
	if err = r.initializeCommandRecovery(ctx); err == nil {
		err = r.initializeCommandInstallations(ctx)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
