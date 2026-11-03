// Package uptimefixture supplies synthetic probe evidence for notification
// contract tests. It opens no network connections and is not an uptime service.
// A new fixture instance needs a new producer epoch; counters are in memory.
package uptimefixture

import (
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type Observation struct {
	Subject   string
	Revision  uint64
	At, Until time.Time
	Reachable bool
}

type Producer struct {
	identity                  protocol.Producer
	subjects                  []string
	observations              map[string]Observation
	retired                   map[string]bool
	sequence, basis, messages uint64
	lastTime                  time.Time
	signature                 string
	snapshotHash              string
}

func New(identity protocol.Producer, subjects []string) (*Producer, error) {
	if !identity.Valid() || len(subjects) == 0 || len(subjects) >= protocol.MaxSubjects {
		return nil, fmt.Errorf("invalid synthetic uptime enrollment")
	}
	subjects = slices.Clone(subjects)
	slices.Sort(subjects)
	for i, key := range subjects {
		if key == "uptime" || !protocol.ValidToken(key) || i > 0 && subjects[i-1] == key {
			return nil, fmt.Errorf("invalid synthetic uptime subject")
		}
	}
	return &Producer{identity: identity, subjects: subjects, observations: map[string]Observation{}, retired: map[string]bool{}}, nil
}

func (p *Producer) Observe(v Observation) error {
	if !slices.Contains(p.subjects, v.Subject) || p.retired[v.Subject] || !protocol.Sequence(v.Revision) || v.At.IsZero() || !v.Until.After(v.At) || v.Until.Sub(v.At) > time.Minute {
		return fmt.Errorf("invalid synthetic probe")
	}
	if old, ok := p.observations[v.Subject]; ok {
		if protocol.Hash(old) == protocol.Hash(v) {
			return nil
		}
		if v.Revision <= old.Revision || v.At.Before(old.At) {
			return fmt.Errorf("probe revision regressed or conflicted")
		}
	}
	p.observations[v.Subject] = v
	return nil
}

func (p *Producer) Retire(subject string) error {
	if !slices.Contains(p.subjects, subject) {
		return fmt.Errorf("unknown synthetic uptime subject")
	}
	p.retired[subject] = true
	return nil
}

func (p *Producer) Evaluate(now time.Time) (protocol.Snapshot, error) {
	if now.IsZero() || !p.lastTime.IsZero() && now.Before(p.lastTime) {
		return protocol.Snapshot{}, fmt.Errorf("synthetic uptime clock regressed")
	}
	var subjects []protocol.Subject
	var facts [][2]string
	aggregate := protocol.Subject{Key: "uptime", Condition: "ok", Reason: "SYNTHETIC uptime evidence only", ValidUntil: now.Add(time.Minute)}
	active := 0
	for _, key := range p.subjects {
		v, exists := p.observations[key]
		s := protocol.Subject{Key: key, Condition: "unknown", Reason: "SYNTHETIC: fresh probe unavailable", ValidUntil: now.Add(30 * time.Second)}
		if p.retired[key] {
			s.Condition, s.Reason = "retired", "SYNTHETIC: explicitly retired target"
		} else {
			active++
			if exists && !v.At.After(now) && v.Until.After(now) {
				s.Condition, s.Reason, s.ValidUntil = "critical", "SYNTHETIC: target unreachable", v.Until
				if v.Reachable {
					s.Condition, s.Reason = "ok", "SYNTHETIC: target reachable"
				}
			}
			if s.Condition == "critical" || s.Condition == "unknown" && aggregate.Condition == "ok" {
				aggregate.Condition = s.Condition
			}
			if s.ValidUntil.Before(aggregate.ValidUntil) {
				aggregate.ValidUntil = s.ValidUntil
			}
		}
		subjects = append(subjects, s)
		facts = append(facts, [2]string{key, s.Condition})
	}
	if active == 0 {
		aggregate.Condition = "retired"
	}
	signature := protocol.Hash(facts)
	basis := p.basis
	if signature != p.signature {
		basis++
	}
	v := protocol.Snapshot{Producer: p.identity, Sequence: p.sequence + 1, BasisRevision: basis, ConfigRevision: protocol.Hash(p.subjects), EvaluatedAt: now, Subjects: append([]protocol.Subject{aggregate}, subjects...)}
	if err := v.Validate(); err != nil {
		return protocol.Snapshot{}, err
	}
	p.basis, p.sequence, p.signature, p.lastTime = basis, v.Sequence, signature, now
	p.snapshotHash = protocol.Hash(v)
	return v, nil
}

// Report has its own delivery sequence and report key. It pins the evaluated
// basis and may only be accepted while that status dependency is still current.
func (p *Producer) Report(snapshot protocol.Snapshot, generation, route, period string) (protocol.Message, error) {
	if snapshot.Producer != p.identity || snapshot.Sequence != p.sequence || snapshot.BasisRevision != p.basis || protocol.Hash(snapshot) != p.snapshotHash || snapshot.Validate() != nil || !protocol.ValidToken(period) {
		return protocol.Message{}, fmt.Errorf("synthetic report needs current evaluation")
	}
	until := snapshot.Subjects[0].ValidUntil
	for _, sub := range snapshot.Subjects {
		if sub.ValidUntil.Before(until) {
			until = sub.ValidUntil
		}
	}
	v := protocol.Message{Producer: p.identity, Sequence: p.messages + 1, ID: fmt.Sprintf("uptime-report-%d", p.messages+1), Generation: generation, Subject: "uptime", Kind: "report", Severity: "info", ObservedAt: snapshot.EvaluatedAt, ExpiresAt: until, Text: "SYNTHETIC uptime report; no network probes were run.", Route: route, ReportKey: "uptime-" + period, ReportType: "daily", ReportRevision: p.messages + 1, BasisRevision: p.basis}
	if err := v.Validate(); err != nil {
		return protocol.Message{}, err
	}
	p.messages++
	return v, nil
}
