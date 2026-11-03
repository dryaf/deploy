package worker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Retained mappings are evidence of identity, never permission to reissue a
// command. A confirmation request may map to a different original request key.
type retainedRequest struct {
	Key, InputHash, ID string
	Command            *interaction.Command
	CommandState       string
	Query              *gatewayQuery
}
type recoveryReader interface{ QueryRow(string, ...any) *sql.Row }

func readRetainedRequest(db recoveryReader, in interaction.Inbound) (m *retainedRequest, err error) {
	m = &retainedRequest{Key: in.RequestKey()}
	err = db.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", m.Key).Scan(&m.InputHash, &m.ID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw string
	if strings.HasPrefix(m.ID, "Q-") {
		m.Query = &gatewayQuery{}
		if err = db.QueryRow("SELECT body FROM gateway_queries WHERE id=?", m.ID).Scan(&raw); err != nil {
			return nil, err
		}
		err = protocol.Decode([]byte(raw), m.Query, protocol.MaxRequest)
	} else {
		m.Command = &interaction.Command{}
		if err = db.QueryRow("SELECT body,state FROM commands WHERE id=?", m.ID).Scan(&raw, &m.CommandState); err != nil {
			return nil, err
		}
		err = protocol.Decode([]byte(raw), m.Command, protocol.MaxRequest)
	}
	if err != nil {
		return nil, err
	}
	if err = m.validate(in); err != nil {
		return nil, err
	}
	if c := m.Command; c != nil {
		var input, id string
		if err = db.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", c.RequestKey).Scan(&input, &id); err != nil {
			return nil, err
		}
		if input != c.InputHash || id != c.ID {
			return nil, fmt.Errorf("retained primary request mapping changed")
		}
	}
	return m, nil
}
func (m retainedRequest) validate(in interaction.Inbound) error {
	if m.Key != in.RequestKey() || m.InputHash != protocol.Hash(in) || (m.Command == nil) == (m.Query == nil) {
		return fmt.Errorf("retained request mapping mismatch")
	}
	if q := m.Query; q != nil {
		expected := in
		if q.Kind == "rejected" && q.Input.Text == "" {
			expected.Text = ""
		}
		if q.ID != m.ID || m.ID != "Q-"+m.Key || protocol.Hash(q.Input) != protocol.Hash(expected) {
			return fmt.Errorf("retained query identity changed")
		}
		return nil
	}
	c := m.Command
	if c.ID != m.ID || !strings.HasPrefix(c.ID, "C-") || !safefs.DigestPattern.MatchString(c.RequestKey) ||
		!safefs.DigestPattern.MatchString(c.InputHash) || !c.Producer.Valid() || c.Scope.Validate() != nil ||
		c.IssuedAt.IsZero() || !c.Deadline.After(c.IssuedAt) || c.Actor != in.Actor ||
		c.Destination.Provider != in.Provider || c.Destination.Account != in.Account ||
		c.Destination.Fingerprint() != in.DestinationFingerprint || (c.RequestKey == m.Key && c.InputHash != m.InputHash) {
		return fmt.Errorf("retained command identity changed")
	}
	return nil
}
func importRequestKey(tx *sql.Tx, key, input, id string) error {
	var oldInput, oldID string
	err := tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", key).Scan(&oldInput, &oldID)
	if err == nil {
		if oldInput != input || oldID != id {
			return fmt.Errorf("restored request mapping conflicts with retained history")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", key, input, id)
	return err
}
func importRetainedRequest(tx *sql.Tx, m retainedRequest) error {
	var raw string
	if c := m.Command; c != nil {
		err := tx.QueryRow("SELECT body FROM commands WHERE id=?", c.ID).Scan(&raw)
		if err == nil {
			var old interaction.Command
			if err = protocol.Decode([]byte(raw), &old, protocol.MaxRequest); err != nil {
				return err
			}
			if protocol.Hash(old) != protocol.Hash(c) {
				return fmt.Errorf("restored command conflicts with retained history")
			}
		} else if err == sql.ErrNoRows {
			body, _ := json.Marshal(c)
			if _, err = tx.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES(?,?,?,?,?,'outcome_unknown',?)", c.ID, c.RequestKey, c.InputHash, c.Producer.ID, string(body), c.IssuedAt.UnixNano()); err != nil {
				return err
			}
		} else {
			return err
		}
		if err = importRequestKey(tx, c.RequestKey, c.InputHash, c.ID); err != nil {
			return err
		}
	} else {
		q := m.Query
		err := tx.QueryRow("SELECT body FROM gateway_queries WHERE id=?", q.ID).Scan(&raw)
		if err == nil {
			var old gatewayQuery
			if err = protocol.Decode([]byte(raw), &old, protocol.MaxRequest); err != nil {
				return err
			}
			if protocol.Hash(old) != protocol.Hash(q) {
				return fmt.Errorf("restored query conflicts with retained history")
			}
		} else if err == sql.ErrNoRows {
			body, _ := json.Marshal(q)
			if _, err = tx.Exec("INSERT INTO gateway_queries VALUES(?,?)", q.ID, string(body)); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return importRequestKey(tx, m.Key, m.InputHash, m.ID)
}
