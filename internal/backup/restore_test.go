//go:build linux

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRestoreContractRejectsIneffectiveAndWrongInput(t *testing.T) {
	root := t.TempDir()
	positive := filepath.Join(root, "positive")
	if err := os.Mkdir(positive, 0700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	good := ApplicationCheckResult{1, digest, true, true, "app-v1", "schema-1"}
	noOp := func(context.Context, string) (ApplicationCheckResult, error) { return good, nil }
	if _, err := applicationCheckSuite(context.Background(), root, positive, digest, noOp); err == nil {
		t.Fatal("no-op checker accepted")
	}
	checker := func(_ context.Context, dir string) (ApplicationCheckResult, error) {
		if dir != positive {
			return ApplicationCheckResult{}, &applicationCheckError{Kind: "application-rejected", Cause: errors.New("schema absent")}
		}
		return good, nil
	}
	if _, err := applicationCheckSuite(context.Background(), root, positive, digest, checker); err != nil {
		t.Fatal(err)
	}
	good.InputSHA256 = strings.Repeat("b", 64)
	if _, err := applicationCheckSuite(context.Background(), root, positive, digest, checker); err == nil {
		t.Fatal("wrong-capture checker accepted")
	}
	timedOut := func(context.Context, string) (ApplicationCheckResult, error) {
		return ApplicationCheckResult{}, context.DeadlineExceeded
	}
	if _, err := applicationCheckSuite(context.Background(), root, positive, digest, timedOut); err == nil {
		t.Fatal("timeout counted as negative rejection")
	}
}
