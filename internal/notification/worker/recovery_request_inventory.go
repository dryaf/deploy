package worker

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type recoveredRequest struct {
	Key, InputHash, TargetID, TargetHash string
}

type RecoveryRequestSummary struct {
	MissingSignalRows                             uint64 `json:",omitempty"`
	Version                                       int
	Operation                                     string
	Identity                                      protocol.Identity
	Requests, Mapped, Unmapped, InboxRows, Opaque uint64
	EvidenceDigest, RequestsDigest                string
	IntakeEnabled                                 bool
}

// Inventory only immutable identities and inputs. Command outcomes may be
// installed separately, but routing, deadlines and original mappings cannot change.
func (r *recoveryReconciler) requestInventory(tx *sql.Tx) (summary RecoveryRequestSummary, records []recoveredRequest, err error) {
	archive := &requestSnapshotEvidence{r: r}
	defer archive.close()
	summary = RecoveryRequestSummary{Version: 1, Operation: r.journal.Operation, Identity: r.journal.After}
	byKey := map[string]recoveredRequest{}
	var evidence []string
	rows, err := tx.Query("SELECT request_key,input_hash,command_id FROM command_inbox ORDER BY request_key")
	if err != nil {
		return
	}
	for rows.Next() {
		var v recoveredRequest
		if err = rows.Scan(&v.Key, &v.InputHash, &v.TargetID); err != nil {
			break
		}
		if !safefs.DigestPattern.MatchString(v.Key) || !safefs.DigestPattern.MatchString(v.InputHash) {
			err = fmt.Errorf("invalid recovered request identity")
			break
		}
		var raw string
		if strings.HasPrefix(v.TargetID, "Q-") {
			var q gatewayQuery
			if err = tx.QueryRow("SELECT body FROM gateway_queries WHERE id=?", v.TargetID).Scan(&raw); err != nil {
				break
			}
			if err = protocol.Decode([]byte(raw), &q, protocol.MaxRequest); err != nil {
				break
			}
			if q.ID != v.TargetID || q.ID != "Q-"+v.Key || q.Input.RequestKey() != v.Key {
				err = fmt.Errorf("recovered query identity changed")
				break
			}
			if !(q.Kind == "rejected" && q.Input.Text == "") && protocol.Hash(q.Input) != v.InputHash {
				err = fmt.Errorf("recovered query input changed")
				break
			}
			v.TargetHash = protocol.Hash(q)
		} else {
			var c interaction.Command
			var key, hash, producer string
			if err = tx.QueryRow("SELECT body,request_key,input_hash,producer FROM commands WHERE id=?", v.TargetID).Scan(&raw, &key, &hash, &producer); err != nil {
				break
			}
			if err = protocol.Decode([]byte(raw), &c, protocol.MaxRequest); err != nil {
				break
			}
			var primaryHash, primaryID string
			if err = tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", c.RequestKey).Scan(&primaryHash, &primaryID); err != nil {
				break
			}
			if !c.Producer.Valid() || c.Scope.Validate() != nil || c.IssuedAt.IsZero() || !c.Deadline.After(c.IssuedAt) || c.ID != v.TargetID || c.ID != "C-"+c.RequestKey || c.RequestKey != key || c.InputHash != hash || c.Producer.ID != producer || primaryID != c.ID || primaryHash != c.InputHash || (v.Key == c.RequestKey && v.InputHash != c.InputHash) {
				err = fmt.Errorf("recovered command mapping changed")
				break
			}
			v.TargetHash = protocol.Hash(c)
		}
		byKey[v.Key] = v
		evidence = append(evidence, protocol.Hash(v))
		if len(byKey) > 65536 {
			err = fmt.Errorf("request recovery inventory too large")
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return
	}
	var missing int
	if err = tx.QueryRow("SELECT count(*) FROM commands c WHERE NOT EXISTS(SELECT 1 FROM command_inbox i WHERE i.request_key=c.request_key AND i.command_id=c.id AND i.input_hash=c.input_hash)").Scan(&missing); err != nil {
		return
	}
	if missing != 0 {
		return summary, nil, fmt.Errorf("recovered command lost primary mapping")
	}
	ancestors, err := r.ancestorRequestFences(tx, byKey)
	if err != nil {
		return summary, nil, err
	}
	evidence = append(evidence, ancestors...)
	rows, err = tx.Query("SELECT account,id,hash,body,state FROM provider_inbox ORDER BY account,id")
	if err != nil {
		return
	}
	defer rows.Close()
	bytes := 0
	for rows.Next() {
		var account, hash, raw, state string
		var id int64
		if err = rows.Scan(&account, &id, &hash, &raw, &state); err != nil {
			return
		}
		summary.InboxRows++
		bytes += len(raw) + 512
		if summary.InboxRows > 65536 || bytes > 32<<20 {
			return summary, nil, fmt.Errorf("request recovery inbox exceeds bound")
		}
		if id < 0 || state != "recovery_quarantined" || !safefs.DigestPattern.MatchString(hash) {
			return summary, nil, fmt.Errorf("request recovery inbox fence changed")
		}
		evidence = append(evidence, protocol.Hash([]any{account, id, hash, raw, state}))
		var in *interaction.Inbound
		var key string
		if raw != "" {
			in = &interaction.Inbound{}
			if err = protocol.Decode([]byte(raw), in, 16384); err != nil {
				return
			}
			if in.Account != account || !protocol.ValidToken(in.NativeID) || (in.Provider != "telegram" && in.Provider != "signal") || (in.Provider == "telegram" && in.NativeID != fmt.Sprintf("update-%d", id)) {
				return summary, nil, fmt.Errorf("recovered inbound identity changed")
			}
			key = in.RequestKey()
			if in.Provider == "signal" {
				event, proof, e := r.requestSignalEvidence(tx, account, id, hash)
				if e == sql.ErrNoRows {
					proof, e = archive.missingSignal(tx, account, id, hash, raw)
					if e == nil {
						summary.MissingSignalRows++
						evidence = append(evidence, proof)
					}
				} else if e == nil {
					if event.Inbound == nil || event.Key != in.NativeID || protocol.Hash(event.Inbound) != protocol.Hash(in) {
						return summary, nil, fmt.Errorf("Signal request differs from retained relay evidence")
					}
					evidence = append(evidence, proof)
				}
				if e != nil {
					return summary, nil, e
				}
			}
		} else {
			summary.Opaque++
			var provider string
			if r.config.Interaction != nil {
				for _, d := range r.config.Interaction.Destinations {
					if d.Account == account {
						if provider != "" && provider != d.Provider {
							return summary, nil, fmt.Errorf("ambiguous recovery account")
						}
						provider = d.Provider
					}
				}
			}
			switch provider {
			case "telegram":
				key = (interaction.Inbound{Provider: provider, Account: account, NativeID: fmt.Sprintf("update-%d", id)}).RequestKey()
			case "signal":
				// Redacted gateway rows retain a spool position, not a native request ID.
				// Require retained relay evidence rather than inventing a key from position.

				event, proof, e := r.requestSignalEvidence(tx, account, id, hash)
				if e == sql.ErrNoRows {
					proof, e = archive.missingSignal(tx, account, id, hash, raw)
					if e != nil {
						return summary, nil, e
					}
					summary.MissingSignalRows++
					evidence = append(evidence, proof)
					// Native identity is unavailable: bind the opaque row without
					// inventing a request key or clearing existing mappings.
					continue
				}
				if e != nil {
					return summary, nil, e
				}
				key = (interaction.Inbound{Provider: provider, Account: account, NativeID: event.Key}).RequestKey()
				in = event.Inbound
				evidence = append(evidence, proof)
			default:
				return summary, nil, fmt.Errorf("redacted recovery account has no provider identity")
			}
		}
		v := byKey[key]
		if v.Key == "" {
			v.Key = key
		}
		if in != nil {
			if v.InputHash != "" && v.InputHash != protocol.Hash(in) {
				return summary, nil, fmt.Errorf("recovered input differs from request mapping")
			}
			v.InputHash = protocol.Hash(in)
			if v.TargetID != "" {
				if _, err = readRetainedRequest(tx, *in); err != nil {
					return
				}
			}
		}
		byKey[key] = v
		if len(byKey) > 65536 {
			return summary, nil, fmt.Errorf("request recovery inventory too large")
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var hashes []string
	for _, key := range keys {
		v := byKey[key]
		records = append(records, v)
		hashes = append(hashes, protocol.Hash(v))
		if v.TargetID != "" {
			summary.Mapped++
		} else {
			summary.Unmapped++
		}
	}
	summary.Requests = uint64(len(records))
	summary.EvidenceDigest = interaction.CommandRecoveryDigest(evidence)
	summary.RequestsDigest = interaction.CommandRecoveryDigest(hashes)
	return
}

func (r *recoveryReconciler) requestSignalEvidence(tx *sql.Tx, account string, id int64, hash string) (event interaction.SpoolEvent, proof string, err error) {
	var body, eventHash, native string
	if err = tx.QueryRow("SELECT key,hash,body FROM reconciliation_signal_events WHERE account=? AND position=?", account, id).Scan(&native, &eventHash, &body); err != nil {
		return
	}
	if err = protocol.Decode([]byte(body), &event, protocol.MaxRequest); err != nil {
		return
	}
	s, err := readSignalRecovery(tx, account)
	if err != nil {
		return
	}
	if err = r.validateSignalSummary(s); err != nil {
		return
	}
	if interaction.ValidateSignalRecoveryEvent(event, s.Challenge.Relay) != nil || event.Key != native || event.Position != uint64(id) || protocol.Hash(event) != eventHash || event.Hash != hash {
		return event, "", fmt.Errorf("Signal request lacks retained identity")
	}
	return event, protocol.Hash([]string{protocol.Hash(s.Summary), eventHash}), nil
}
