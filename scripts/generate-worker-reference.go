//go:build ignore

// Refresh only the generated field-inventory section; preserve the operator guide.
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/workerctl"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate() error {
	const path = "docs/reference/worker-configuration.md"
	section, err := workerctl.WorkerFieldReference()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	startMarker, endMarker := []byte("<!-- worker-field-inventory:start -->"), []byte("<!-- worker-field-inventory:end -->")
	start, end := bytes.Index(raw, startMarker), bytes.Index(raw, endMarker)
	if start < 0 && end < 0 {
		raw = append(raw, []byte("\n## Complete public field inventory\n\n"+section)...)
	} else {
		if start < 0 || end < start || bytes.Count(raw, startMarker) != 1 || bytes.Count(raw, endMarker) != 1 {
			return fmt.Errorf("field reference markers are ambiguous")
		}
		end += len(endMarker)
		if end < len(raw) && raw[end] == '\n' {
			end++
		}
		updated := append([]byte{}, raw[:start]...)
		updated = append(updated, section...)
		raw = append(updated, raw[end:]...)
	}
	return os.WriteFile(path, raw, 0644)
}
