package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type requestSealCrashInput struct {
	Path   string
	Config Config
}

func TestRequestRecoverySealCrashChild(t *testing.T) {
	file := os.Getenv("REQUEST_SEAL_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var input requestSealCrashInput
	must(t, json.Unmarshal(raw, &input))
	session, err := OpenRequestRecoverySession(context.Background(), input.Path, input.Config)
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(at string) error {
		if at == os.Getenv("REQUEST_SEAL_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.SealRecoveryRequests(context.Background())
	must(t, err)
	t.Fatal("fault missing")
}
func TestRequestRecoverySealSIGKILL(t *testing.T) {
	for _, stage := range []string{"request_seal_intent", "request_seal_transaction", "request_seal_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, r := requestSealFixture(t)
			must(t, r.Close())
			path := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(requestSealCrashInput{f.Path, f.Config})
			must(t, err)
			must(t, os.WriteFile(path, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequestRecoverySealCrashChild$")
			cmd.Env = append(os.Environ(), "REQUEST_SEAL_INPUT="+path, "REQUEST_SEAL_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("fault missing", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenRequestRecoverySession(ctx, f.Path, f.Config)
			must(t, err)
			defer session.Close()
			resumed := session.(*recoveryReconciler)
			var completed, records int
			must(t, resumed.db.QueryRow("SELECT count(*) FROM reconciliation_request_seal").Scan(&completed))
			must(t, resumed.db.QueryRow("SELECT count(*) FROM reconciliation_request_fences").Scan(&records))
			if stage == "request_seal_commit" {
				if completed != 1 || records != 7 {
					t.Fatal("lost committed inventory", completed, records)
				}
			} else if completed != 0 || records != 0 {
				t.Fatal("partial seal visible", completed, records)
			}
			a, err := session.SealRecoveryRequests(ctx)
			must(t, err)
			b, err := session.SealRecoveryRequests(ctx)
			must(t, err)
			if a != b || a.Requests != 7 {
				t.Fatal("restart changed seal")
			}
		})
	}
}
