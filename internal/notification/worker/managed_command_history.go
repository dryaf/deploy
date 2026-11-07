package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const managedCommandHeadKey = "managed-command-head"

type managedCommandHead struct {
	Version                    int
	Identity                   protocol.Identity
	Hash, Mode, ActivationHash string
	Policy                     string `json:",omitempty"`
}
type managedCommandTransition struct {
	Before   *managedCommandHead
	After    managedCommandHead
	Recovery *ManagedCommandRecovery `json:"recovery,omitempty"`
	Grant    bool                    `json:"grant,omitempty"`
}

func (h managedCommandHead) valid() bool {
	return ((h.Version == 1 && h.Policy == "") || ((h.Version == 2 || h.Version == 3) && h.Version == commandPolicyVersion(h.Policy) && validManagedCommandPolicy(h.Policy))) && h.Identity.Valid() && safefs.DigestPattern.MatchString(h.Hash) && safefs.DigestPattern.MatchString(h.ActivationHash) && (h.Mode == "read" || h.Mode == "all" || h.Mode == "drain")
}
func sameCommandHead(a, b *managedCommandHead) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Store) commandSQLHead() (*managedCommandHead, error) {
	var raw string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", managedCommandHeadKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h managedCommandHead
	if protocol.Decode([]byte(raw), &h, 65536) != nil || !h.valid() {
		return nil, fmt.Errorf("invalid command SQL authority")
	}
	return &h, nil
}

func commandFileHead(d *safefs.Directory) (*managedCommandHead, error) {
	var h managedCommandHead
	err := d.ReadJSON("head.json", &h)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !h.valid() {
		return nil, fmt.Errorf("invalid command file authority")
	}
	return &h, nil
}

func verifyCommandHistory(d *safefs.Directory, head *managedCommandHead, ignore string) error {
	allowed := map[string]bool{"head.json": true}
	if ignore != "" {
		allowed[ignore+".json"] = true
		allowed[ignore+"-complete.json"] = true
	}
	current := head
	for n := 0; current != nil; n++ {
		if n >= 1024 || !current.valid() {
			return fmt.Errorf("invalid command authority history")
		}
		name := current.Hash + ".json"
		if allowed[name] {
			return fmt.Errorf("cyclic command authority history")
		}
		allowed[name] = true
		allowed[current.Hash+"-complete.json"] = true
		var transition, complete managedCommandTransition
		if err := d.ReadJSON(name, &transition); err != nil {
			return err
		}
		if err := d.ReadJSON(current.Hash+"-complete.json", &complete); err != nil {
			return err
		}
		if transition.After != *current || protocol.Hash(transition) != protocol.Hash(complete) || !validCommandTransition(transition) {
			return fmt.Errorf("command authority history changed")
		}
		current = transition.Before
	}
	for {
		entries, err := d.File().ReadDir(128)
		for _, entry := range entries {
			if !allowed[entry.Name()] || !entry.Type().IsRegular() {
				return fmt.Errorf("unacknowledged or later command authority evidence")
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// RequireManagedCommandHistory detects restored installation pointers, missing
// SQL/file anchors, missing history prefixes and retained later disable records.
func (s *Store) RequireManagedCommandHistory(hash string) error {
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	return s.checkManagedCommandHistory(hash)
}

// Read-only snapshot preflight compares both authorities without acquiring a
// runtime lease. Publication repeats this check under the exclusive lease.
func (s *Store) checkManagedCommandHistory(hash string) error {
	if hash != "" && !safefs.DigestPattern.MatchString(hash) {
		return fmt.Errorf("invalid command history pin")
	}
	sqlHead, err := s.commandSQLHead()
	if err != nil {
		return err
	}
	d, err := s.dir.Sub("managed-commands")
	if os.IsNotExist(err) && hash == "" && sqlHead == nil {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	fileHead, err := commandFileHead(d)
	if err != nil {
		return err
	}
	if !sameCommandHead(fileHead, sqlHead) || (hash == "") != (sqlHead == nil) || sqlHead != nil && (sqlHead.Hash != hash || sqlHead.Identity != s.identity) {
		return fmt.Errorf("managed command authority differs from installation")
	}
	if err = verifyCommandHistory(d, fileHead, ""); err != nil {
		return err
	}
	return validateCommandEnrollmentHistory(d, fileHead, s.config.Interaction, "")
}

// CommitManagedCommandAdmission runs only after the service stops and while the
// installer retains exclusive installation and worker leases. The complete file
// makes loss of either acknowledged head an error, never a repair opportunity.
func (s *Store) CommitManagedCommandAdmission(mode, hash, previous string) error {
	return s.commitManagedCommandAdmission(mode, hash, previous, nil)
}

func (s *Store) commitManagedCommandAdmission(mode, hash, previous string, fault func(string) error) error {
	return s.commitManagedCommandPolicy(mode, hash, previous, "", fault)
}

func (s *Store) commitManagedCommandPolicy(mode, hash, previous, policy string, fault func(string) error, grants ...bool) error {
	grant := len(grants) == 1 && grants[0]
	if len(grants) > 1 || grant && policy == "" {
		return fmt.Errorf("invalid explicit command grant")
	}
	after := managedCommandHead{Version: 1, Identity: s.identity, Hash: hash, Mode: mode, ActivationHash: s.deliveryActivationHash}
	if s.verificationOnly || s.installationRevision == "" || s.config.Interaction == nil || !after.valid() || previous != "" && !safefs.DigestPattern.MatchString(previous) || previous == hash {
		return fmt.Errorf("invalid command admission transition")
	}
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	sqlHead, err := s.commandSQLHead()
	if err != nil {
		return err
	}
	d, err := s.dir.Sub("managed-commands")
	if os.IsNotExist(err) && sqlHead == nil && previous == "" {
		if err = s.dir.Mkdir("managed-commands"); err != nil {
			return err
		}
		d, err = s.dir.Sub("managed-commands")
	}
	if err != nil {
		return err
	}
	defer d.Close()
	fileHead, err := commandFileHead(d)
	if err != nil {
		return err
	}
	var intent managedCommandTransition
	err = d.ReadJSON(hash+".json", &intent)
	if os.IsNotExist(err) {
		if !sameCommandHead(sqlHead, fileHead) || (previous == "") != (sqlHead == nil) || sqlHead != nil && (sqlHead.Hash != previous || sqlHead.Identity != s.identity) {
			return fmt.Errorf("command transition previous authority changed")
		}
		if err = verifyCommandHistory(d, fileHead, ""); err != nil {
			return err
		}
		if fileHead != nil {
			after.Policy = fileHead.Policy
		}
		if policy != "" {
			if fileHead == nil || fileHead.Mode != "drain" || mode != "drain" {
				return fmt.Errorf("narrowing requires disabled commands")
			}
			if err = validateCommandEnrollmentHistory(d, fileHead, s.config.Interaction, policy, grant); err != nil {
				return err
			}
			after.Policy = policy
		}
		if after.Policy != "" {
			after.Version = commandPolicyVersion(after.Policy)
		}
		intent = managedCommandTransition{Before: fileHead, After: after, Grant: grant}
		if err = d.WriteJSON(hash+".json", intent, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Derive inherited policy from the retained intent, including interrupted commits.
	if intent.Before != nil {
		after.Policy = intent.Before.Policy
	}
	if policy != "" {
		after.Policy = policy
	}
	if after.Policy != "" {
		after.Version = commandPolicyVersion(after.Policy)
	}
	if policy != "" {
		if intent.Before == nil || intent.Before.Mode != "drain" || mode != "drain" {
			return fmt.Errorf("narrowing requires disabled commands")
		}
		if _, err = changedCommandConfig(s.config.Interaction, intent.Before.Policy, policy, grant); err != nil {
			return err
		}
	}
	if intent.Grant != grant || !validCommandTransition(intent) || intent.After != after || (intent.Before == nil) != (previous == "") || intent.Before != nil && (!intent.Before.valid() || intent.Before.Hash != previous || intent.Before.Identity != s.identity) {
		return fmt.Errorf("command transition intent changed")
	}
	audit, err := s.dir.Sub("managed-commands")
	if err != nil {
		return err
	}
	err = verifyCommandHistory(audit, intent.Before, hash)
	audit.Close()
	if err != nil {
		return err
	}
	var complete managedCommandTransition
	err = d.ReadJSON(hash+"-complete.json", &complete)
	if err == nil {
		if protocol.Hash(complete) != protocol.Hash(intent) || !sameCommandHead(sqlHead, &after) || !sameCommandHead(fileHead, &after) {
			return fmt.Errorf("completed command transition lost authority")
		}
		return s.RequireManagedCommandHistory(hash)
	} else if !os.IsNotExist(err) {
		return err
	}
	if !sameCommandHead(sqlHead, intent.Before) && !sameCommandHead(sqlHead, &after) || !sameCommandHead(fileHead, intent.Before) && !sameCommandHead(fileHead, &after) || sameCommandHead(fileHead, &after) && !sameCommandHead(sqlHead, &after) {
		return fmt.Errorf("inconsistent command transition progress")
	}
	if err = recoveryBoundary(fault, "command-intent"); err != nil {
		return err
	}
	if !sameCommandHead(sqlHead, &after) {
		raw, _ := json.Marshal(after)
		if err = s.transaction(context.Background(), func(tx *sql.Tx) error {
			if policy != "" && commandPolicyVersion(policy) == 3 {
				next, e := changedCommandConfig(s.config.Interaction, intent.Before.Policy, policy, grant)
				if e != nil {
					return e
				}
				if e = s.fenceCommandEnrollment(tx, next); e != nil {
					return e
				}
			}
			_, e := tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", managedCommandHeadKey, string(raw))
			return e
		}); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "command-sql"); err != nil {
		return err
	}
	if !sameCommandHead(fileHead, &after) {
		if err = d.WriteJSON("head.json", after, fileHead != nil); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "command-head"); err != nil {
		return err
	}
	if err = d.WriteJSON(hash+"-complete.json", intent, false); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "command-complete"); err != nil {
		return err
	}
	return s.RequireManagedCommandHistory(hash)
}
