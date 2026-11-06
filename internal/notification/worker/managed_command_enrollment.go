package worker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Enrollment overlays preserve the canonical owner/recovery configuration. They
// can retire conversations/workers, rename aliases and rotate a conversation's
// generation, but cannot expand permissions or redirect an existing audience.
func decodeCommandEnrollment(raw string) (*interaction.Config, error) {
	var c interaction.Config
	if len(raw) > 16384 || protocol.Decode([]byte(raw), &c, 16384) != nil || c.Validate() != nil {
		return nil, fmt.Errorf("invalid command enrollment configuration")
	}
	b, err := json.Marshal(c)
	if err != nil || string(b) != raw {
		return nil, fmt.Errorf("noncanonical command enrollment configuration")
	}
	return &c, nil
}

func enrollmentSubset(before, after *interaction.Config) bool {
	if before == nil || after == nil || after.Validate() != nil || !permissionsSubset(before.Permissions, after.Permissions) {
		return false
	}
	for _, next := range after.Workers {
		found := false
		for _, old := range before.Workers {
			if old.Producer == next.Producer {
				old.Alias = next.Alias
				found = protocol.Hash(old) == protocol.Hash(next)
			}
		}
		if !found {
			return false
		}
	}
	for _, next := range after.Destinations {
		found := false
		for _, old := range before.Destinations {
			if old.ID == next.ID {
				old.Generation = next.Generation
				found = old == next
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func commandPolicyVersion(raw string) int {
	if raw == "" {
		return 1
	}
	if _, err := decodeCommandEnrollment(raw); err == nil {
		return 3
	}
	return 2
}

func validateCommandEnrollmentHistory(d *safefs.Directory, head *managedCommandHead, base *interaction.Config, proposed string) error {
	if proposed != "" && commandPolicyVersion(proposed) == 3 && head != nil && proposed == head.Policy {
		return fmt.Errorf("command enrollment is unchanged")
	}
	var policies []string
	for n := 0; head != nil; n++ {
		if n >= 1024 {
			return fmt.Errorf("command enrollment history budget exceeded")
		}
		policies = append(policies, head.Policy)
		var t managedCommandTransition
		if err := d.ReadJSON(head.Hash+".json", &t); err != nil {
			return err
		}
		if t.After != *head || !validCommandTransition(t) {
			return fmt.Errorf("command enrollment history changed")
		}
		head = t.Before
	}
	for i, j := 0, len(policies)-1; i < j; i, j = i+1, j-1 {
		policies[i], policies[j] = policies[j], policies[i]
	}
	if proposed != "" {
		policies = append(policies, proposed)
	}
	if base == nil {
		return fmt.Errorf("command enrollment unavailable")
	}
	aliases := map[string]protocol.Producer{}
	revisions := map[string]bool{base.Revision: true}
	generations := map[string]bool{}
	current := base
	for _, w := range base.Workers {
		aliases[w.Alias] = w.Producer
	}
	for _, dest := range base.Destinations {
		generations[dest.ID+"/"+dest.Generation] = true
	}
	prior := ""
	for _, policy := range policies {
		if policy == prior {
			continue
		}
		next, err := narrowedCommandConfig(base, prior, policy)
		if err != nil {
			return err
		}
		if commandPolicyVersion(policy) == 3 {
			if revisions[next.Revision] {
				return fmt.Errorf("command enrollment revision must be new")
			}
			revisions[next.Revision] = true
			for _, w := range next.Workers {
				if p, ok := aliases[w.Alias]; ok && p != w.Producer {
					return fmt.Errorf("historical alias remains reserved")
				}
				aliases[w.Alias] = w.Producer
			}
			for _, dest := range next.Destinations {
				for _, old := range current.Destinations {
					if old.ID == dest.ID && old.Generation != dest.Generation {
						key := dest.ID + "/" + dest.Generation
						if generations[key] {
							return fmt.Errorf("retired conversation generation cannot be reused")
						}
						generations[key] = true
					}
				}
			}
		}
		current, prior = next, policy
	}
	return nil
}

// CommandDestinations returns the effective intake enrollment, including retained
// removals. Call after ConfigureManagedCommands and before starting any loops.
func (s *Store) CommandDestinations() []interaction.Destination {
	if s.commandConfig() == nil {
		return nil
	}
	return append([]interaction.Destination(nil), s.commandConfig().Destinations...)
}

// Freeze every persisted, unconverted provider input in the same transaction as
// the new configuration head. An input received under an old alias can never be
// converted using a later alias, even after restore or direct adapter replay.
func (s *Store) fenceCommandEnrollment(tx *sql.Tx, next *interaction.Config) error {
	if err := s.reserveAliases(tx); err != nil {
		return err
	}
	prior := s.managedCommandPolicy
	s.managedCommandPolicy = next
	err := s.reserveAliases(tx)
	s.managedCommandPolicy = prior
	if err != nil {
		return err
	}
	rows, err := tx.Query("SELECT body FROM provider_inbox WHERE state='pending' LIMIT 1025")
	if err != nil {
		return err
	}
	var inputs []interaction.Inbound
	for rows.Next() {
		var raw string
		var in interaction.Inbound
		if err = rows.Scan(&raw); err == nil {
			err = protocol.Decode([]byte(raw), &in, 8192)
		}
		if err != nil {
			rows.Close()
			return err
		}
		inputs = append(inputs, in)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(inputs) > 1024 {
		return fmt.Errorf("pending command input budget exceeded")
	}
	for _, in := range inputs {
		var existing string
		err = tx.QueryRow("SELECT input_hash FROM command_inbox WHERE request_key=?", in.RequestKey()).Scan(&existing)
		if err == nil {
			if existing != protocol.Hash(in) {
				return fmt.Errorf("pending input conflicts with retained command mapping")
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		q := gatewayQuery{ID: "Q-" + in.RequestKey(), Input: in, Policy: protocol.Hash(next), Expires: s.now().Add(10 * time.Minute), Kind: "rejected"}
		body, err := json.Marshal(q)
		if err != nil {
			return err
		}
		if err = s.capacity(tx, "", len(body)+8192, false); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO gateway_queries VALUES(?,?)", q.ID, string(body)); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", in.RequestKey(), protocol.Hash(in), q.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE provider_inbox SET state='rejected' WHERE state='pending'")
	return err
}
