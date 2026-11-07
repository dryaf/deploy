//go:build linux

package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

func testProductionLEDCLI(t *testing.T, root string, policy func(bool, string), write func(string, string), read func(*testing.T, string) []byte, run func(*testing.T, string, string, bool) map[string]any) {
	for _, commands := range []bool{false, true} {
		name := "delivery"
		if commands {
			name = "commands"
		}
		t.Run(name+"-led-output", func(t *testing.T) {
			ctx := context.Background()
			startBackupCLILED(t, root+"/led.sock")
			write("led-facts", "synthetic hardware inventory")
			defer os.Remove(root + "/led-facts")
			policy(true, "daily")
			base := string(read(t, "server.yaml"))
			fragment := "    notification_worker:\n      status_leds:\n        enabled: true\n        socket: /tmp/wux/led.sock\n        daemon: {mode: existing}\n        bindings:\n          backup: {producer: backup-worker, subjects: ['*'], led: green}\n"
			if commands {
				fragment += "      commands:\n        enabled: true\n        mode: mutation\n        workers: {backup: backup-worker}\n        conversations:\n          ops: {provider: telegram, account: '123', native_id: '456'}\n        permissions:\n          - {actor: '789', conversation: ops, worker: backup, permissions: [read, incident.manage], scope: {whole_worker: true}}\n"
				base = strings.Replace(base, "        telegram:\n", "        telegram:\n          account: '123'\n          bot_name: FixtureBot\n", 1)
			}
			declared := strings.Replace(base, "    notification_worker:\n", fragment, 1)
			write("server.yaml", declared)
			run(t, "backup-worker", "apply", true)
			const setup = "manager/.local/state/deploy/backup-worker/setup.json"
			var before backup.WorkerSetupJournal
			testutil.Must(t, json.Unmarshal(read(t, setup), &before))
			const coordinator = "manager/.local/state/deploy/worker-control/notification-worker/"
			var enrolled OperationJournal
			testutil.Must(t, json.Unmarshal(read(t, coordinator+"operation.json"), &enrolled))
			gateway := enrolled.Pins.Installations["notification-worker"]
			// The real short qualification command uses a terminal in this isolated
			// fixture. All observations and daemon details are explicitly synthetic.
			observed, err := install.InspectInstallation(ctx, gateway)
			testutil.Must(t, err)
			session, err := install.BeginQualification(ctx, gateway, protocol.Hash(observed.Manifest))
			testutil.Must(t, err)
			if !session.LEDEnvironmentRequired {
				t.Fatal("short qualification hid required LED environment")
			}
			write("led-config.json", "synthetic offline daemon configuration")
			write("evidence.log", "synthetic offline observations; no hardware or live provider")
			answers := "synthetic-v1\n" + root + "/led-config.json\n"
			for range session.Checks {
				answers += "passed\n" + root + "/evidence.log\n"
			}
			qualifyNotificationThroughTerminal(t, root, answers)

			admitted := run(t, "notification-worker", "apply", true)
			wantState := "delivery_admitted"
			if commands {
				wantState = "commands_admitted"
			}
			if admitted["state"] != wantState && (commands || admitted["state"] != "reporting_connected") {
				t.Fatal(admitted)
			}
			original, err := install.InspectInstallation(ctx, gateway)
			testutil.Must(t, err)
			enrollment := read(t, coordinator+"consumer-enrollment.json")
			identity := read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")
			var receiver []byte
			const receiverPath = "manager/.local/state/deploy/backup-worker/state/command-receiver.json"
			if commands {
				receiver = read(t, receiverPath)
			}
			for _, enabled := range []bool{false, true, false} {
				next := declared
				if !enabled {
					next = strings.Replace(next, "      status_leds:\n        enabled: true", "      status_leds:\n        enabled: false", 1)
				}
				write("server.yaml", next)
				run(t, "notification-worker", "plan", true)
				updated := run(t, "notification-worker", "apply", true)
				if updated["state"] != wantState {
					t.Fatal(updated)
				}
				for _, worker := range []string{"backup-worker", "notification-worker"} {
					for _, action := range []string{"apply", "resume", "status", "plan"} {
						run(t, worker, action, true)
					}
				}
				after, err := install.InspectInstallation(ctx, gateway)
				testutil.Must(t, err)
				expected := original.Manifest
				expected.LEDOutput = after.Manifest.LEDOutput
				if after.Manifest.LEDOutput == nil || after.Manifest.LEDOutput.Disabled == enabled || protocol.Hash(expected) != protocol.Hash(after.Manifest) {
					t.Fatal("output changed delivery/command ownership")
				}
				var setupAfter backup.WorkerSetupJournal
				testutil.Must(t, json.Unmarshal(read(t, setup), &setupAfter))
				if setupAfter.WorkerID != before.WorkerID || setupAfter.ArchiveID != before.ArchiveID || !reflect.DeepEqual(setupAfter.Producer, before.Producer) || !reflect.DeepEqual(setupAfter.SourceKeys, before.SourceKeys) {
					t.Fatal("output replaced backup identities")
				}
				if !bytes.Equal(enrollment, read(t, coordinator+"consumer-enrollment.json")) || !bytes.Equal(identity, read(t, "manager/.local/state/deploy/notification-worker/state/identity.json")) {
					t.Fatal("output replaced enrollment")
				}
				if commands && !bytes.Equal(receiver, read(t, receiverPath)) {
					t.Fatal("output replaced original receiver")
				}
				entries, err := (led.Client{Socket: root + "/led.sock", Budget: 2 * time.Second}).List(ctx)
				testutil.Must(t, err)
				foreign := false
				for _, entry := range entries {
					if entry.ID == "foreign-fault" {
						foreign = true
					}
				}
				if !foreign {
					t.Fatal("output cleared foreign LED state")
				}
				if !commands {
					qualifyNotificationThroughTerminal(t, root, answers)
					paused, err := install.InspectInstallation(ctx, gateway)
					testutil.Must(t, err)
					if paused.Manifest.Mode != "verification" || *paused.Manifest.LEDOutput != *after.Manifest.LEDOutput {
						t.Fatal("renewal lost selected output permission")
					}
					run(t, "notification-worker", "apply", true)
					for _, worker := range []string{"backup-worker", "notification-worker"} {
						for _, action := range []string{"apply", "resume", "status", "plan"} {
							run(t, worker, action, true)
						}
					}
					renewed, err := install.InspectInstallation(ctx, gateway)
					testutil.Must(t, err)
					if renewed.Manifest.Identity != original.Manifest.Identity || renewed.Manifest.DeliveryOwnerHash != original.Manifest.DeliveryOwnerHash || *renewed.Manifest.LEDOutput != *after.Manifest.LEDOutput {
						t.Fatal("LED renewal changed ownership")
					}
					original = renewed
				}
			}
			if commands {
				next := string(read(t, "server.yaml"))
				disabled := strings.Replace(next, "      commands:\n        enabled: true", "      commands:\n        enabled: false", 1)
				for _, body := range []string{disabled, next} {
					write("server.yaml", body)
					run(t, "notification-worker", "apply", true)
					for _, worker := range []string{"backup-worker", "notification-worker"} {
						for _, action := range []string{"resume", "status", "plan"} {
							run(t, worker, action, true)
						}
					}
					if !bytes.Equal(receiver, read(t, receiverPath)) {
						t.Fatal("command toggle after LED output replaced receiver")
					}
				}
			}
			cmd := exec.Command(root+"/systemctl", "--user", "stop", "deploy-notification-worker.service")
			if out, err := process.LimitedOutput(cmd, 45*time.Second, 4096); err != nil {
				t.Fatal(err, string(out))
			}
			for _, dir := range []string{"manager", "app"} {
				testutil.Must(t, os.Rename(root+"/"+dir, root+"/"+dir+"-"+name+"-led-output"))
				testutil.Must(t, os.Mkdir(root+"/"+dir, 0700))
			}
		})
		if t.Failed() {
			return
		}
	}
}

// A terminal is required by the production wizard. This adapter supplies only
// fixture observations to the compiled command, with ordinary SSH transport.
func qualifyNotificationThroughTerminal(t *testing.T, root, answers string, ledEnvironment ...bool) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"args": []string{root + "/deploy", "--servers", root + "/server.yaml", "notification-worker", "qualify", "manager", "--json"}, "answers": answers})
	testutil.Must(t, err)
	script := `import json,os,pty,subprocess,sys,termios
request=json.load(sys.stdin)
master,slave=pty.openpty()
attrs=termios.tcgetattr(slave)
attrs[3] &= ~termios.ECHO
termios.tcsetattr(slave,termios.TCSANOW,attrs)
p=subprocess.Popen(request['args'],stdin=slave,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
os.close(slave)
try:
 data=request['answers'].encode()
 while data:
  written=os.write(master,data)
  data=data[written:]
 out,err=p.communicate(timeout=180)
 sys.stdout.buffer.write(out)
 sys.stderr.buffer.write(err)
 sys.exit(p.returncode)
finally:
 if p.poll() is None:
  p.kill()
  p.wait()
 os.close(master)
`
	cmd := exec.Command("python3", "-I", "-c", script)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr process.Buffer
	stdout.Limit, stderr.Limit = 1<<20, 1<<16
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = process.Run(cmd, 4*time.Minute, time.Second); err != nil {
		t.Fatal(err, stdout.String(), stderr.String())
	}
	var result ControlResult
	testutil.Must(t, json.Unmarshal(stdout.Bytes(), &result))
	if result.QualificationSession == nil || !result.QualificationSession.Recorded || !result.QualificationSession.EvidenceComplete || result.QualificationSession.LEDEnvironmentRequired {
		t.Fatal("short wizard did not record complete synthetic qualification", result, stderr.String())
	}
	if (len(ledEnvironment) == 0 || ledEnvironment[0]) && !strings.Contains(stderr.String(), "Observed LED daemon version") {
		t.Fatal("wizard omitted LED environment prompt")
	}
}
