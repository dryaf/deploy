package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxMessageDecisions = 1024

// MessageReview is a short-lived administrative observation, not an execution
// grant or a producer receipt. It contains the complete immutable message so an
// operator can review precisely which destination-bound event is being changed.
type MessageReview struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	Identity      protocol.Identity `json:"identity"`
	Job           Job               `json:"job"`
	JobHash       string            `json:"job_hash"`
	ConfigHash    string            `json:"config_hash"`
	OwnershipHash string            `json:"ownership_hash"`
	ReceiptHead   int64             `json:"receipt_head"`
	ActorUID      int               `json:"actor_uid"`
	CreatedAt     time.Time         `json:"created_at"`
	ExpiresAt     time.Time         `json:"expires_at"`
	BootID        string            `json:"boot_id"`
	Uptime        float64           `json:"uptime"`
}

type MessageDecision struct {
	Version             int           `json:"version"`
	Review              MessageReview `json:"review"`
	Action              string        `json:"action"`
	Reason              string        `json:"reason"`
	AcceptDuplicateRisk bool          `json:"accept_duplicate_risk"`
}

type MessageDecisionRecord struct {
	Version    int              `json:"version"`
	Decision   MessageDecision  `json:"decision"`
	Receipt    protocol.Receipt `json:"receipt"`
	RecordedAt time.Time        `json:"recorded_at"`
}

type messageAdministrator struct {
	s     *Store
	clock func() (string, float64, error)
	fault func(string) error
}

func (s *Store) ReviewMessage(ctx context.Context, p protocol.Producer, id string) (MessageReview, error) {
	return (messageAdministrator{s: s, clock: process.HostClock}).review(ctx, p, id)
}
func (s *Store) DecideMessage(ctx context.Context, d MessageDecision) (MessageDecisionRecord, error) {
	return (messageAdministrator{s: s, clock: process.HostClock}).decide(ctx, d)
}
func (s *Store) MessageDecision(ctx context.Context, id string) (MessageDecisionRecord, error) {
	return (messageAdministrator{s: s, clock: process.HostClock}).status(ctx, id)
}

func (v MessageReview) Validate() error {
	if v.Version != 1 || !safefs.ValidID(v.ID, "n") || !v.Identity.Valid() || !v.Job.Message.Producer.Valid() || !protocol.ValidToken(v.Job.Message.ID) || !protocol.Sequence(v.Job.Message.Sequence) || v.JobHash != protocol.Hash(v.Job) || !safefs.DigestPattern.MatchString(v.ConfigHash) || !safefs.DigestPattern.MatchString(v.OwnershipHash) || v.ReceiptHead < 0 || v.ActorUID < 0 || v.CreatedAt.IsZero() || !v.ExpiresAt.Equal(v.CreatedAt.Add(5*time.Minute)) || !protocol.ValidToken(v.BootID) || v.Uptime < 0 || math.IsNaN(v.Uptime) || math.IsInf(v.Uptime, 0) {
		return fmt.Errorf("invalid administrative message review")
	}
	if v.Job.Message.Validate() != nil || v.Job.Tries < 0 || v.Job.Attempt != "" || !slices.Contains([]string{"queued", "waiting_status", "retry_wait", "fenced", "ambiguous", "blocked"}, v.Job.State) {
		return fmt.Errorf("message is not eligible for administrative review")
	}
	return nil
}
func (d MessageDecision) Validate() error {
	if err := d.Review.Validate(); err != nil {
		return err
	}
	if d.Version != 1 || (d.Action != "retry" && d.Action != "cancel") || !utf8.ValidString(d.Reason) || strings.TrimSpace(d.Reason) == "" || len(d.Reason) > 1024 || strings.ContainsAny(d.Reason, "\x00\r\n") || (d.Action == "cancel" && d.AcceptDuplicateRisk) {
		return fmt.Errorf("invalid message decision or reason")
	}
	if d.Action == "retry" && d.Review.Job.State != "blocked" && d.Review.Job.State != "ambiguous" {
		return fmt.Errorf("only blocked or ambiguous messages can be retried")
	}
	if d.Action == "retry" && d.Review.Job.State == "ambiguous" && !d.AcceptDuplicateRisk {
		return fmt.Errorf("retrying an ambiguous message requires explicit duplicate-risk acknowledgement")
	}
	return nil
}

func messageDecisionKey(id string) string { return "message-decision-v1:" + id }
func messageIntentFile(id string) string  { return "administration/" + id + ".intent.json" }
func messageResultFile(id string) string  { return "administration/" + id + ".result.json" }

func (a messageAdministrator) boundary(point string) error {
	if a.fault != nil {
		return a.fault(point)
	}
	return nil
}

func messageReceiptHead(ctx context.Context, tx *sql.Tx, p protocol.Producer, id string) (int64, error) {
	var head int64
	err := tx.QueryRowContext(ctx, "SELECT coalesce(max(position),0) FROM receipts WHERE producer=? AND json_extract(body,'$.event_id')=?", p.ID, id).Scan(&head)
	return head, err
}

func (a messageAdministrator) review(ctx context.Context, p protocol.Producer, id string) (v MessageReview, err error) {
	if !protocol.ValidToken(id) {
		return v, fmt.Errorf("invalid event ID")
	}
	err = a.s.ownership(p, func(o *ownership) error {
		return a.s.transaction(ctx, func(tx *sql.Tx) error {
			job, e := readJob(tx, p.ID, id)
			if e != nil {
				return e
			}
			if terminal(job.State) || job.Attempt != "" {
				return fmt.Errorf("only nonterminal messages without an in-flight attempt can be reviewed")
			}
			boot, up, e := a.clock()
			if e != nil {
				return e
			}
			ref, e := safefs.NewID("n")
			if e != nil {
				return e
			}
			head, e := messageReceiptHead(ctx, tx, p, id)
			if e != nil {
				return e
			}
			now := a.s.now().UTC()
			v = MessageReview{Version: 1, ID: ref, Identity: a.s.identity, Job: job, JobHash: protocol.Hash(job), ConfigHash: protocol.Hash(a.s.config), OwnershipHash: protocol.Hash(o), ReceiptHead: head, ActorUID: os.Getuid(), CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), BootID: boot, Uptime: up}
			return v.Validate()
		})
	})
	return
}

func (a messageAdministrator) currentReview(ctx context.Context, tx *sql.Tx, o ownership, d MessageDecision) (Job, error) {
	v := d.Review
	if v.Identity != a.s.identity || v.ConfigHash != protocol.Hash(a.s.config) || v.OwnershipHash != protocol.Hash(o) || v.ActorUID != os.Getuid() {
		return Job{}, fmt.Errorf("review identity, configuration, ownership or operator changed")
	}
	boot, up, err := a.clock()
	if err != nil {
		return Job{}, err
	}
	now := a.s.now().UTC()
	delta := up - v.Uptime
	if boot != v.BootID || math.IsNaN(up) || math.IsInf(up, 0) || delta < 0 || delta >= 300 {
		return Job{}, fmt.Errorf("message review expired or host clock changed")
	}
	wall, elapsed := now.Sub(v.CreatedAt), time.Duration(delta*float64(time.Second))
	if now.Before(v.CreatedAt) || !now.Before(v.ExpiresAt) || wall-elapsed > 2*time.Second || elapsed-wall > 2*time.Second {
		return Job{}, fmt.Errorf("message review expired or host clock changed")
	}
	j, err := readJob(tx, v.Job.Message.Producer.ID, v.Job.Message.ID)
	if err != nil {
		return j, err
	}
	head, err := messageReceiptHead(ctx, tx, j.Message.Producer, j.Message.ID)
	if err != nil {
		return j, err
	}
	if protocol.Hash(j) != v.JobHash || head != v.ReceiptHead || j.Attempt != "" || terminal(j.State) {
		return j, fmt.Errorf("message or its receipt history changed; obtain a new review")
	}
	if d.Action == "retry" {
		if j.State != "blocked" && j.State != "ambiguous" {
			return j, fmt.Errorf("only blocked or ambiguous messages can be retried")
		}
		g, e := a.s.deliveryGeneration(tx, o, j)
		if e != nil {
			return j, e
		}
		if o.Generations[g] != "active" && o.Generations[g] != "staged" {
			return j, fmt.Errorf("retry cannot revive a revoked or draining generation")
		}
		route, ok := a.s.config.Routes[j.Message.Route]
		if !ok || route.Revision != j.RouteRevision {
			return j, fmt.Errorf("retry cannot redirect a message to a changed route")
		}
		eligible, e := a.s.eligibility(tx, j.Message, 0)
		if e != nil {
			return j, e
		}
		if !eligible.Eligible && terminal(eligible.Reason) {
			return j, fmt.Errorf("message is expired or superseded; retry cannot extend its validity")
		}
	}
	return j, nil
}

// decide is an offline administrative operation under the worker lease. It is
// deliberately absent from the producer Unix API. The SQL mutation, audit record
// and unsuccessful/pending receipt commit together; the file result records that
// same completion and never grants a provider attempt.
func (a messageAdministrator) decide(ctx context.Context, d MessageDecision) (result MessageDecisionRecord, err error) {
	if err = d.Validate(); err != nil {
		return
	}
	if d.Review.Identity != a.s.identity {
		return result, fmt.Errorf("administrative review belongs to another worker epoch")
	}
	p := d.Review.Job.Message.Producer
	err = a.s.ownership(p, func(o *ownership) error {
		return a.s.transaction(ctx, func(tx *sql.Tx) error {
			var exists bool
			var e error
			result, exists, e = a.readRecord(ctx, tx, d.Review.ID)
			if e != nil {
				return e
			}
			if exists {
				if protocol.Hash(result.Decision) != protocol.Hash(d) {
					return fmt.Errorf("decision ID already used for a different instruction")
				}
				return nil
			}
			j, e := a.currentReview(ctx, tx, *o, d)
			if e != nil {
				return e
			}
			var recorded MessageDecision
			fileErr := a.s.dir.ReadJSON(messageIntentFile(d.Review.ID), &recorded)
			if fileErr == nil && protocol.Hash(recorded) != protocol.Hash(d) {
				return fmt.Errorf("administrative intent conflict")
			}
			if fileErr != nil && !os.IsNotExist(fileErr) {
				return fileErr
			}
			body, e := json.Marshal(d)
			if e != nil {
				return e
			}
			if e = a.s.capacity(tx, p.ID, 2*len(body)+16384, false); e != nil {
				return e
			}
			if e = a.reserveHistory(ctx, tx, d.Review.ID); e != nil {
				return e
			}
			if os.IsNotExist(fileErr) {
				if e = a.s.dir.Mkdir("administration"); e != nil {
					return e
				}
				if e = a.s.dir.WriteJSON(messageIntentFile(d.Review.ID), d, false); e != nil {
					return e
				}
			}
			if e = a.boundary("administration_intent"); e != nil {
				return e
			}
			if _, e = a.currentReview(ctx, tx, *o, d); e != nil {
				return e
			}
			state := "cancelled"
			if d.Action == "retry" {
				state = "queued"
				if protocol.Guarded(j.Message.Kind) {
					state = "waiting_status"
				}
			}
			if _, e = tx.ExecContext(ctx, "UPDATE messages SET state=?,due=0,provider=CASE WHEN ?='retry' THEN '' ELSE provider END WHERE producer=? AND id=?", state, d.Action, p.ID, j.Message.ID); e != nil {
				return e
			}
			now := a.s.now().UTC()
			receipt := protocol.Receipt{EventID: j.Message.ID, Sequence: j.Message.Sequence, State: state, At: now}
			if e = a.s.receipt(tx, p.ID, receipt); e != nil {
				return e
			}
			if e = tx.QueryRowContext(ctx, "SELECT last_insert_rowid()").Scan(&receipt.Position); e != nil {
				return e
			}
			result = MessageDecisionRecord{Version: 1, Decision: d, Receipt: receipt, RecordedAt: now}
			if e = result.Validate(); e != nil {
				return e
			}
			b, e := json.Marshal(result)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", messageDecisionKey(d.Review.ID), string(b)); e != nil {
				return e
			}
			return a.boundary("administration_transaction")
		})
	})
	if err != nil {
		return result, err
	}
	if err = a.boundary("administration_commit"); err != nil {
		return
	}
	err = a.publishResult(result)
	return
}
