package workerctl

import (
	"os"
	"strings"
	"testing"
)

func TestWorkerFieldInventoryMatchesPublicYAMLAndReference(t *testing.T) {
	fields, err := WorkerFieldInventory()
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) < 100 {
		t.Fatal("incomplete public field traversal", len(fields))
	}
	for _, field := range fields {
		if field.Runtime == "" || field.Execution == "" || field.Transition == "installation_review" {
			t.Fatalf("unaccounted field: %+v", field)
		}
	}
	want, err := WorkerFieldReference()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../docs/reference/worker-configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "<!-- worker-field-inventory:start -->")
	end := strings.Index(text, "<!-- worker-field-inventory:end -->")
	if start < 0 || end < start {
		t.Fatal("missing checked field reference; run go run ./scripts/generate-worker-reference.go")
	}
	end += len("<!-- worker-field-inventory:end -->\n")
	if end > len(text) || text[start:end] != want {
		t.Fatal("public field reference is stale; run go run ./scripts/generate-worker-reference.go")
	}
}
