package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type ledPolicyEdge struct {
	before  Config
	after   string
	mapping *ledMappingIntent
}

// Policy and mapping edits can alternate. Resolve the old output configuration
// through exact, independently anchored domain receipts, never by dropping fields
// from the current configuration. Every usable edge pins the same output record.
func (s *Store) ledOutputValidationConfig(ctx context.Context, q ledRowReader, intent ledOutputIntent) (Config, error) {
	if protocol.Hash(s.config) == intent.ConfigHash {
		return s.config, nil
	}
	record, err := readLEDOutputRecord(ctx, q, intent.Request.Operation)
	if err != nil || !record.Complete || protocol.Hash(record.Intent) != protocol.Hash(intent) {
		return Config{}, fmt.Errorf("LED policy handoff requires the retained completed output record")
	}
	pin := protocol.Hash(record)
	var edges []ledPolicyEdge
	addHash := func(before Config, after string, mapping *ledMappingIntent, operation, hash string) error {
		if hash == "" {
			return nil
		}
		r, e := readLEDOutputRecord(ctx, q, operation)
		if e != nil || !r.Complete || r.Intent.Request.Identity != s.identity || r.Intent.Request.Operation != operation || protocol.Hash(r) != hash {
			return fmt.Errorf("LED policy lost pinned output history")
		}
		if operation == intent.Request.Operation && hash == pin && protocol.Hash(before) != after {
			edges = append(edges, ledPolicyEdge{before, after, mapping})
		}
		return nil
	}
	add := func(before, after Config, operation, hash string) error {
		return addHash(before, protocol.Hash(after), nil, operation, hash)
	}
	enrolled, err := readEnrolledLimits(s.dir)
	if err != nil {
		return Config{}, err
	}
	var pending *enrolledLimits
	if enrolled != nil && !enrolled.Complete {
		pending = enrolled
	}
	if err = checkEnrolledLimitsSQL(ctx, s.dir, q, enrolled, pending); err != nil {
		return Config{}, err
	}
	for v, count := enrolled, 0; v != nil && count < 64; count++ {
		if v.Receipt.Identity != s.identity {
			return Config{}, fmt.Errorf("enrolled LED identity changed")
		}
		if v.Complete {
			if err = add(v.Receipt.Before, v.Receipt.After, v.Receipt.LEDOutputOperation, v.Receipt.LEDOutputHash); err != nil {
				return Config{}, err
			}
		}
		if v.Receipt.Previous == "" {
			break
		}
		var prior enrolledLimits
		if err = s.dir.ReadJSON("enrolled-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return Config{}, err
		}
		if prior.validate() != nil || !prior.Complete || prior.digest() != v.Receipt.Previous {
			return Config{}, fmt.Errorf("enrolled LED predecessor changed")
		}
		v = &prior
	}
	var positions int
	if err = q.QueryRowContext(ctx, "SELECT COALESCE(max(position),0) FROM led_migrations").Scan(&positions); err != nil {
		return Config{}, err
	}
	if positions > maxLEDMigrations {
		return Config{}, fmt.Errorf("LED mapping history exceeds bound")
	}
	for position := positions; position > 0; position-- {
		var op string
		if err = q.QueryRowContext(ctx, "SELECT id FROM led_migrations WHERE position=?", position).Scan(&op); err != nil {
			return Config{}, err
		}
		m, e := readLEDMapping(ctx, q, op)
		if e != nil {
			return Config{}, e
		}
		if m.Intent.ID != op || m.Intent.Position != position {
			return Config{}, fmt.Errorf("LED mapping position changed")
		}
		// Version one predates output ownership and cannot authorize projection of
		// an existing output record. Its layout history is still audited on open.
		if m.Intent.Version != 2 {
			continue
		}
		if m.Intent.OutputBeforeConfig == nil {
			return Config{}, fmt.Errorf("LED mapping predecessor missing")
		}
		before := *m.Intent.OutputBeforeConfig
		if err = validateLEDMapping(m, before, s.identity); err != nil {
			return Config{}, err
		}
		if m.Completed {
			if err = addHash(before, m.Intent.AfterConfigHash, &m.Intent, m.Intent.LEDOutputOperation, m.Intent.LEDOutputHash); err != nil {
				return Config{}, err
			}
		}
	}
	var enrollment bootstrapEnrollment
	err = s.dir.ReadJSON("bootstrap-enrollment.json", &enrollment)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}
	if err == nil {
		if enrollment.validate() != nil || enrollment.Receipt.Identity != s.identity {
			return Config{}, fmt.Errorf("LED enrollment authority changed")
		}
		var anchor string
		e := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return Config{}, e
		}
		if anchor != enrollment.digest() && (enrollment.Complete || anchor != "") {
			return Config{}, fmt.Errorf("LED enrollment SQL authority changed")
		}
		if enrollment.Complete {
			before := enrollment.Config
			before.Producers = nil
			if enrollment.Receipt.Version == 3 {
				before.Interaction = nil
			}
			if protocol.Hash(before) != enrollment.Receipt.Before {
				return Config{}, fmt.Errorf("LED enrollment changed unrelated policy")
			}
			if err = add(before, enrollment.Config, enrollment.Receipt.LEDOutputOperation, enrollment.Receipt.LEDOutputHash); err != nil {
				return Config{}, err
			}
		}
	}
	bootstrap, err := readBootstrapLimits(s.dir)
	if err != nil {
		return Config{}, err
	}
	var anchor string
	err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", bootstrapLimitsKey).Scan(&anchor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Config{}, err
	}
	if bootstrap == nil && anchor != "" || bootstrap != nil && anchor != bootstrap.digest() && (bootstrap.Complete || anchor != bootstrap.Receipt.Previous) {
		return Config{}, fmt.Errorf("bootstrap LED policy SQL authority changed")
	}
	for v, count := bootstrap, 0; v != nil && count < 32; count++ {
		if v.Receipt.Identity != s.identity {
			return Config{}, fmt.Errorf("bootstrap LED policy identity changed")
		}
		if v.Complete {
			if err = add(v.Receipt.Before, v.Receipt.After, v.Receipt.LEDOutputOperation, v.Receipt.LEDOutputHash); err != nil {
				return Config{}, err
			}
		}
		if v.Receipt.Previous == "" {
			break
		}
		var prior bootstrapLimits
		if err = s.dir.ReadJSON("bootstrap-limits-history/"+v.Receipt.Previous+".json", &prior); err != nil {
			return Config{}, err
		}
		if prior.validate() != nil || !prior.Complete || prior.digest() != v.Receipt.Previous {
			return Config{}, fmt.Errorf("bootstrap LED policy predecessor changed")
		}
		v = &prior
	}
	// Configurations may intentionally return to earlier values. Search bounded
	// exact edges rather than guessing an order from independent operation IDs.
	queue := []Config{s.config}
	seen := map[string]bool{}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		hash := protocol.Hash(c)
		if hash == intent.ConfigHash {
			return c, nil
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		for _, edge := range edges {
			if edge.after == hash {
				if edge.mapping != nil {
					v := edge.mapping
					if ledBaseConfigHash(c) != v.BaseConfigHash || protocol.Hash(desiredLEDLayout(c, s.identity.Worker, v.After.UID).Bindings) != protocol.Hash(v.After.Bindings) {
						return Config{}, fmt.Errorf("LED mapping changed unrelated policy or layout")
					}
				}
				queue = append(queue, edge.before)
			}
		}
	}
	return Config{}, fmt.Errorf("LED output configuration lacks an exact policy/mapping handoff")
}
