package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// ManagedCommandRecovery carries the latest independently retained command
// history into a new epoch as drain-only evidence. It never admits commands.
type ManagedCommandRecovery struct {
	Version      int               `json:"version"`
	Before       protocol.Identity `json:"before"`
	After        protocol.Identity `json:"after"`
	CommitHash   string            `json:"commit_hash"`
	PreviousHash string            `json:"previous_hash"`
	OwnerHash    string            `json:"owner_hash"`
}

func (r ManagedCommandRecovery) Validate() error {
	if r.Version != 1 || !r.Before.Valid() || !r.After.Valid() || r.Before.Worker != r.After.Worker || r.Before.Recovery == r.After.Recovery || !safefs.DigestPattern.MatchString(r.CommitHash) || !safefs.DigestPattern.MatchString(r.PreviousHash) || !safefs.DigestPattern.MatchString(r.OwnerHash) {
		return fmt.Errorf("invalid managed command recovery")
	}
	return nil
}

func (r ManagedCommandRecovery) head(policy string) managedCommandHead {
	h := managedCommandHead{Version: 1, Identity: r.After, Hash: protocol.Hash(r), Mode: "drain", ActivationHash: r.CommitHash, Policy: policy}
	if policy != "" {
		h.Version = commandPolicyVersion(policy)
	}
	return h
}

func validCommandTransition(t managedCommandTransition) bool {
	if !t.After.valid() || t.Before != nil && !t.Before.valid() {
		return false
	}
	if t.Recovery == nil {
		if t.Before == nil {
			return t.After.Policy == ""
		}
		return t.Before.Identity == t.After.Identity && (t.Before.Policy == t.After.Policy || (t.Before.Mode == "drain" && t.After.Mode == "drain" && policySubset(t.Before.Policy, t.After.Policy)))
	}
	r := *t.Recovery
	return r.Validate() == nil && t.Before != nil && t.Before.Identity == r.Before && t.Before.Hash == r.PreviousHash && t.After == r.head(t.Before.Policy)
}

func commandHistoryContains(d *safefs.Directory, head, wanted *managedCommandHead) bool {
	if wanted == nil {
		return true
	}
	for n := 0; head != nil && n < 1024; n++ {
		if *head == *wanted {
			return true
		}
		var t managedCommandTransition
		if d.ReadJSON(head.Hash+".json", &t) != nil || !validCommandTransition(t) || t.After != *head {
			return false
		}
		head = t.Before
	}
	return false
}

func (s *Store) EnrollManagedCommandRecovery(r ManagedCommandRecovery, delivery ManagedDeliveryRecovery) error {
	return s.enrollManagedCommandRecovery(r, delivery, nil)
}

func (s *Store) enrollManagedCommandRecovery(r ManagedCommandRecovery, delivery ManagedDeliveryRecovery, fault func(string) error) error {
	if !s.verificationOnly || s.installationRevision == "" || r.Validate() != nil || r.Before != delivery.Before || r.After != s.identity || r.After != delivery.After || r.CommitHash != s.recoveryCommitHash || r.OwnerHash != protocol.Hash(delivery.Owner) || (delivery.Version == 1 && delivery.Owner.Identity != r.Before) {
		return fmt.Errorf("command recovery requires original managed ownership and verified publication")
	}
	if err := s.RequireManagedDeliveryRecovery(delivery, r.OwnerHash); err != nil {
		return err
	}
	if err := s.RequireInactiveProducers(); err != nil {
		return err
	}
	ctx := context.Background()
	if err := verifyRetainedDatabase(ctx, s.dir, publicationExchange, s.recoveryPublication.OriginalDatabaseHash, s.recoveryPublication.OriginalDatabaseBytes, s.config.MaxDatabaseBytes); err != nil {
		return err
	}
	db, err := immutableOriginalDB(s.dir, publicationExchange)
	if err != nil {
		return err
	}
	var raw string
	err = db.QueryRow("SELECT value FROM metadata WHERE key=?", managedCommandHeadKey).Scan(&raw)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	var original managedCommandHead
	if protocol.Decode([]byte(raw), &original, 65536) != nil || !original.valid() || original.Identity != r.Before || original.Hash != r.PreviousHash {
		return fmt.Errorf("original source does not prove latest command history")
	}
	d, err := s.dir.Sub("managed-commands")
	if err != nil {
		return err
	}
	defer d.Close()
	sqlHead, err := s.commandSQLHead()
	if err != nil {
		return err
	}
	fileHead, err := commandFileHead(d)
	if err != nil {
		return err
	}
	after := r.head(original.Policy)
	want := managedCommandTransition{Before: &original, After: after, Recovery: &r}
	var intent, complete managedCommandTransition
	err = d.ReadJSON(after.Hash+".json", &intent)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return err
	}
	if !missing && protocol.Hash(intent) != protocol.Hash(want) {
		return fmt.Errorf("command recovery intent changed")
	}
	audit, err := s.dir.Sub("managed-commands")
	if err != nil {
		return err
	}
	err = verifyCommandHistory(audit, &original, after.Hash)
	audit.Close()
	if err != nil {
		return err
	}
	if !sameCommandHead(fileHead, &original) && !sameCommandHead(fileHead, &after) || !sameCommandHead(sqlHead, &after) && !commandHistoryContains(d, &original, sqlHead) || sameCommandHead(fileHead, &after) && !sameCommandHead(sqlHead, &after) {
		return fmt.Errorf("command recovery source/head changed")
	}
	if missing && (sameCommandHead(fileHead, &after) || sameCommandHead(sqlHead, &after)) {
		return fmt.Errorf("command recovery lost intent")
	}
	err = d.ReadJSON(after.Hash+"-complete.json", &complete)
	if err == nil {
		if missing || protocol.Hash(complete) != protocol.Hash(want) || !sameCommandHead(fileHead, &after) || !sameCommandHead(sqlHead, &after) {
			return fmt.Errorf("completed command recovery lost anchors")
		}
		return s.RequireManagedCommandHistory(after.Hash)
	} else if !os.IsNotExist(err) {
		return err
	}
	if missing {
		if err = d.WriteJSON(after.Hash+".json", want, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "command-recovery-intent"); err != nil {
		return err
	}
	if !sameCommandHead(sqlHead, &after) {
		data, _ := json.Marshal(after)
		if _, err = s.db.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", managedCommandHeadKey, string(data)); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "command-recovery-sql"); err != nil {
		return err
	}
	if !sameCommandHead(fileHead, &after) {
		if err = d.WriteJSON("head.json", after, true); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "command-recovery-head"); err != nil {
		return err
	}
	if err = d.WriteJSON(after.Hash+"-complete.json", want, false); err != nil {
		return err
	}
	if err = recoveryBoundary(fault, "command-recovery-complete"); err != nil {
		return err
	}
	return s.RequireManagedCommandHistory(after.Hash)
}
