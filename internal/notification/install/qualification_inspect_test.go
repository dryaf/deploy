package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestQualificationSummaryRetainsPendingAndStaleEvidence(t *testing.T) {
	spec, _, c, report := qualificationFixture(t)
	pin := protocol.Hash(report.Binding.Manifest)
	now := time.Now().UTC()
	result, err := inspectQualificationAt(context.Background(), spec.Root, pin, now)
	if err != nil || result.EvidenceComplete || result.AdmissionValid || result.BindingMatches || len(result.Checks) != len(report.Checks) {
		t.Fatal(result, err)
	}
	for _, check := range result.Checks {
		if check.Result != "pending" {
			t.Fatal(check)
		}
	}
	if _, err = os.Stat(filepath.Join(spec.Root, "qualifications")); !os.IsNotExist(err) {
		t.Fatal("inspection created evidence", err)
	}
	qualificationPass(&report)
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	result, err = inspectQualificationAt(context.Background(), spec.Root, pin, time.Now().UTC())
	if err != nil || !result.EvidenceComplete || !result.BindingMatches || result.AdmissionValid {
		t.Fatal(result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"123:secret", c.Providers["telegram"].TokenFile, report.Checks[0].EvidenceHash, status.RecordHash, pin} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("summary leaked private data")
		}
	}
	for _, at := range []time.Time{report.CreatedAt.Add(-time.Hour), report.CreatedAt.Add(qualificationWindow)} {
		result, err = inspectQualificationAt(context.Background(), spec.Root, pin, at)
		if err != nil || result.EvidenceComplete || result.BindingMatches || result.AdmissionValid {
			t.Fatal(result, err)
		}
		for _, check := range result.Checks {
			if check.Result != "pending" {
				t.Fatal("stale check shown as passing", check)
			}
		}
	}
	if _, err = InspectQualification(context.Background(), spec.Root, protocol.Hash("changed manifest")); err == nil {
		t.Fatal("mixed installation revisions")
	}
	if err = os.WriteFile(c.Providers["telegram"].TokenFile, []byte("123:changed-token"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = inspectQualificationAt(context.Background(), spec.Root, pin, time.Now().UTC())
	if err != nil || result.BindingMatches || result.EvidenceComplete {
		t.Fatal(result, err)
	}
}
