package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Rejected provider events receive a durable terminal mapping as well. A repeated
// update cannot become a new request just because configuration later changes.
func (s *Store) recordCommandRejection(ctx context.Context, in interaction.Inbound, rejection *interaction.Rejection) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		historical, err := recoveredRequestFor(tx, in)
		if err != nil {
			return err
		}
		if historical != nil {
			return nil
		}
		if err = guardSignalRecoveryInput(tx, in); err != nil {
			var rejected *interaction.Rejection
			if errors.As(err, &rejected) && rejected.Silent {
				return nil
			}
			return err
		}
		if err = guardTelegramRecoveryInput(tx, in); err != nil {
			var rejected *interaction.Rejection
			if errors.As(err, &rejected) && rejected.Silent {
				return nil
			}
			return err
		}

		var existing int
		if err := tx.QueryRow("SELECT count(*) FROM command_inbox WHERE request_key=?", in.RequestKey()).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return nil
		}
		d, authorized := s.queryDestination(in)
		reply := authorized && !rejection.Silent
		if reply {
			if err := s.commandRate(tx, in); err != nil {
				var denied *interaction.Rejection
				if !errors.As(err, &denied) {
					return err
				}
				reply = false
			}
		}
		if reply {
			var count, own int
			if err := tx.QueryRow("SELECT count(*),COALESCE(sum(json_extract(g.body,'$.input.actor')=? AND json_extract(g.body,'$.input.account')=?),0) FROM command_replies r JOIN gateway_queries g ON g.id=r.command_id WHERE r.kind='gateway' AND r.state IN ('pending','sending','ambiguous')", in.Actor, in.Account).Scan(&count, &own); err != nil {
				return err
			}
			if count >= 128 || own >= 16 {
				reply = false
			}
		}
		q := gatewayQuery{ID: "Q-" + in.RequestKey(), Input: in, Policy: protocol.Hash(s.commandConfig()), Expires: s.now().Add(10 * time.Minute), Kind: "rejected"}
		if !authorized {
			q.Input.Text = ""
		}
		body, _ := json.Marshal(q)
		if err := s.capacity(tx, "", len(body)+8192, false); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO gateway_queries VALUES(?,?)", q.ID, string(body)); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", in.RequestKey(), protocol.Hash(in), q.ID); err != nil {
			return err
		}
		if !reply {
			return nil
		}
		body, _ = json.Marshal(CommandReply{Kind: "gateway", CommandID: q.ID, Destination: d, Text: "Request rejected: " + rejection.Message})
		_, err = tx.Exec("INSERT INTO command_replies(command_id,kind,revision,body) VALUES(?,'gateway',0,?)", q.ID, string(body))
		return err
	})
}
