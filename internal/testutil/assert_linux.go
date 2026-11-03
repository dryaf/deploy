//go:build linux

package testutil

import (
	"testing"
)

func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
