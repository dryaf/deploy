package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (s *Store) ReconcileCommand(ctx context.Context, p protocol.Producer, proof interaction.NonAdmissionProof) (v interaction.View, err error) {
	if _, err = s.config.enrollment(p); err != nil {
		return v, err
	}
	unsigned := proof
	unsigned.ID = ""
	if proof.Identity != s.identity || proof.Producer != p || proof.ID != protocol.Hash(unsigned) {
		return v, fmt.Errorf("invalid non-admission proof")
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		if err := requireCurrentCommand(tx, proof.CommandID); err != nil {
			return err
		}

		v, err = readCommand(tx, proof.CommandID)
		if err != nil {
			return err
		}
		if v.Command.Producer != p || proof.CommandHash != protocol.Hash(v.Command) || v.Grant == nil || proof.GrantID != v.Grant.ID {
			return fmt.Errorf("non-admission identity mismatch")
		}
		if v.NonAdmission != nil {
			if *v.NonAdmission != proof {
				return fmt.Errorf("conflicting proof")
			}
			return nil
		}
		if !s.clockSafe() {
			return fmt.Errorf("clock uncertain")
		}
		if err = expireCommand(tx, &v, s.now()); err != nil {
			return err
		}
		if v.State != "outcome_unknown" {
			return fmt.Errorf("execution already reported or admission still open")
		}
		if v.Revision != 0 {
			var phase string
			if err = tx.QueryRow("SELECT json_extract(body,'$.phase') FROM command_results WHERE command_id=? AND revision=?", v.Command.ID, v.Revision).Scan(&phase); err != nil {
				return err
			}
			if phase != "prepare" {
				return fmt.Errorf("execution already reported")
			}
		}
		v.State = "expired"
		v.NonAdmission = &proof
		body, _ := json.Marshal(proof)
		if _, err = tx.Exec("UPDATE commands SET state='expired',non_admission=? WHERE id=?", string(body), v.Command.ID); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE command_replies SET state='superseded' WHERE command_id=? AND state='pending'", v.Command.ID); err != nil {
			return err
		}
		return s.queueCommandReplyKind(tx, v.Command, "terminal", v.Revision, "Expired without execution admission", v.Command.Scope)
	})
	return
}
