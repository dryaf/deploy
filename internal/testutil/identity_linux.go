//go:build linux

package testutil

import (
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

func ID(t *testing.T, prefix string) string {
	t.Helper()
	id, err := safefs.NewID(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
