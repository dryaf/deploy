package worker

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

type managedPolicyCrashInput struct {
	Path     string
	Config   Config
	Boundary string
	Policy   string
	Grant    bool
}

func TestManagedCommandPolicyCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_POLICY_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var in managedPolicyCrashInput
	must(t, json.Unmarshal(raw, &in))
	s, err := Open(in.Path, in.Config, false)
	must(t, err)
	defer s.Close()
	s.installationRevision = "n-01234567890123456789012345678901"
	s.deliveryActivationHash = protocol.Hash("delivery")
	policy := in.Policy
	if policy == "" {
		policy = "[]"
	}
	err = s.commitManagedCommandPolicy("drain", protocol.Hash("narrow"), protocol.Hash("disabled"), policy, func(at string) error {
		if at == in.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}, in.Grant)
	t.Fatalf("crash boundary was not reached: %v", err)
}

func TestManagedCommandPolicySIGKILL(t *testing.T)     { testManagedCommandPolicySIGKILL(t, false) }
func TestManagedCommandEnrollmentSIGKILL(t *testing.T) { testManagedCommandPolicySIGKILL(t, true) }
func TestManagedCommandGrantSIGKILL(t *testing.T)      { testManagedCommandPolicySIGKILL(t, true, true) }
func testManagedCommandPolicySIGKILL(t *testing.T, enrollment bool, grants ...bool) {
	grant := len(grants) == 1 && grants[0]
	for _, point := range []string{"command-intent", "command-sql", "command-head", "command-complete"} {
		t.Run(point, func(t *testing.T) {
			s := managedCommandHistoryFixture(t)
			path, c := s.dir.File().Name(), s.config
			policy := "[]"
			if enrollment {
				next := *c.Interaction
				next.Revision = "two"
				next.Workers = append([]interaction.Worker(nil), next.Workers...)
				if !grant {
					next.Workers[0].Alias = "backup/renamed"
				}
				policy = commandEnrollmentJSON(t, &next)
				in := interaction.Inbound{Provider: "telegram", Account: "bot-1", NativeID: "update-1", Actor: "actor-1", Conversation: "private", DestinationFingerprint: c.Interaction.Destinations[0].Fingerprint(), SentAt: s.now(), Text: "backup/storage status"}
				raw, err := json.Marshal(in)
				must(t, err)
				_, err = s.db.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,?)", in.Account, 1, protocol.Hash(in), string(raw), "pending")
				must(t, err)
			}
			if grant {
				must(t, s.CommitManagedCommandAdmission("drain", protocol.Hash("initial"), ""))
				must(t, s.CommitManagedCommandPolicy(protocol.Hash("disabled"), protocol.Hash("initial"), "[]"))
			} else {
				must(t, s.CommitManagedCommandAdmission("drain", protocol.Hash("disabled"), ""))
			}
			must(t, s.Close())
			raw, err := json.Marshal(managedPolicyCrashInput{Path: path, Config: c, Boundary: point, Policy: policy, Grant: grant})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagedCommandPolicyCrashChild$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_POLICY_CRASH_INPUT="+input)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 4096)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("expected SIGKILL: %v %s", err, out)
			}
			s, err = Open(path, c, false)
			must(t, err)
			defer s.Close()
			s.installationRevision = "n-01234567890123456789012345678901"
			s.deliveryActivationHash = protocol.Hash("delivery")
			if s.RequireManagedCommandHistory(protocol.Hash("disabled")) == nil {
				t.Fatal("old authority accepted crashed narrowing")
			}
			if grant {
				must(t, s.CommitManagedCommandGrant(protocol.Hash("narrow"), protocol.Hash("disabled"), policy))
			} else {
				must(t, s.CommitManagedCommandPolicy(protocol.Hash("narrow"), protocol.Hash("disabled"), policy))
			}
			must(t, s.CommitManagedCommandAdmission("all", protocol.Hash("reenable"), protocol.Hash("narrow")))
			must(t, s.ConfigureManagedCommands("all", protocol.Hash("reenable")))
			if enrollment {
				wantAlias := "backup/renamed"
				if grant {
					wantAlias = "backup/storage"
				}
				if s.commandConfig().Workers[0].Alias != wantAlias {
					t.Fatal("migration lost")
				}
				var pending, mappings int
				must(t, s.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE state='pending'").Scan(&pending))
				must(t, s.db.QueryRow("SELECT count(*) FROM command_inbox").Scan(&mappings))
				if pending != 0 || mappings != 1 {
					t.Fatal("input fence lost", pending, mappings)
				}
			} else if len(s.commandConfig().Permissions) != 0 {
				t.Fatal("SIGKILL restored rights")
			}
		})
	}
}
