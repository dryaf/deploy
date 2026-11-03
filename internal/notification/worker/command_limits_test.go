package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCommandBurstSurvivesRestart(t *testing.T) {
	s, now, dir, in := commandFixture(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		in.NativeID = fmt.Sprintf("burst-%d", i)
		_, err := s.AdmitCommand(ctx, in)
		must(t, err)
	}
	config := s.config
	must(t, s.Close())
	r, err := Open(dir, config, false)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return *now }
	_, err = r.AdmitCommand(ctx, in)
	must(t, err) // A retained replay consumes no token.
	in.NativeID = "sixth"
	if _, err = r.AdmitCommand(ctx, in); err == nil {
		t.Fatal("restart reset burst budget")
	}
	*now = now.Add(6 * time.Second)
	_, err = r.AdmitCommand(ctx, in)
	must(t, err)
}

func TestCommandPullByteBudgetRotatesWithoutStarvation(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	// Populate retained envelopes large enough to require multiple wire pages.
	// Admission quotas are tested separately; this exercises pull scheduling.
	for n := 0; n < 20; n++ {
		c := v.Command
		c.ID = fmt.Sprintf("large-%02d", n)
		c.RequestKey = c.ID
		c.Scope = interaction.Scope{}
		for j := 0; j < 128; j++ {
			c.Scope.Subjects = append(c.Scope.Subjects, fmt.Sprintf("subject-%03d-%s", j, strings.Repeat("x", 220)))
		}
		raw, _ := json.Marshal(c)
		_, err = s.db.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES(?,?,?,?,?,'queued',?)", c.ID, c.RequestKey, c.InputHash, producer.ID, string(raw), c.IssuedAt.UnixNano())
		must(t, err)
	}
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		page, err := s.Commands(ctx, producer)
		must(t, err)
		raw, _ := json.Marshal(page)
		if len(raw) > protocol.MaxResponse-1024 {
			t.Fatal("oversized response", len(raw))
		}
		for _, v := range page {
			seen[v.Command.ID] = true
		}
	}
	if len(seen) != 21 {
		t.Fatal("large pending commands starved later requests", len(seen))
	}
}
