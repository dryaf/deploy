package workerctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/ledhost"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/transport"
	"golang.org/x/sys/unix"
)

const maxControlFrame = 1 << 20

type ControlRequest struct {
	SourceTargets      []backup.SetupSourceTarget `json:"source_targets,omitempty"` // Inventory comparison only; never enrollment authority.
	DiagnosticProvider string                     `json:"diagnostic_provider,omitempty"`
	DesiredScope       int                        `json:"desired_scope,omitempty"`
	LocalLED           *Artifact                  `json:"-"`
	LEDSHA256          string                     `json:"led_sha256,omitempty"`
	ledEndpoint        *led.Endpoint              // Target-local read-only preflight; never trusted from the operator frame.
	LocalSignal        *Artifact                  `json:"-"` // Operator-local transfer input; never target policy.
	SignalSHA256       string                     `json:"signal_sha256,omitempty"`
	InteractiveLink    bool                       `json:"interactive_link,omitempty"`
	SignalReview       string                     `json:"signal_review,omitempty"` // Explicit reviewed link operation, never a new link.

	Observation  *install.QualificationObservation `json:"observation,omitempty"`
	Version      int                               `json:"version"`
	Worker       string                            `json:"worker"`
	Server       string                            `json:"server"`
	Action       string                            `json:"action"`
	Facts        TargetFacts                       `json:"facts"`
	Desired      config.DesiredWorkers             `json:"desired"`
	DesiredHash  string                            `json:"desired_hash"`
	HelperSHA256 string                            `json:"helper_sha256"`
	// Inputs travel only in the bounded stdin frame, never argv or log output.
	Inputs map[string][]byte `json:"inputs,omitempty"`
}
type ControlResult struct {
	Import               *ImportReview                 `json:"import,omitempty"`
	LEDHostRemoval       *LEDHostRemoval               `json:"led_host_removal,omitempty"`
	Changes              []Change                      `json:"changes,omitempty"`
	LEDPrerequisite      *LEDPrerequisite              `json:"led_prerequisite,omitempty"`
	LEDOutputPending     bool                          `json:"led_output_pending,omitempty"`
	LEDOutput            *install.ManagedLEDOutput     `json:"led_output,omitempty"`
	SignalReview         string                        `json:"signal_review,omitempty"`
	QualificationSession *install.QualificationSession `json:"qualification_session,omitempty"`
	Qualification        *install.QualificationSummary `json:"qualification,omitempty"`
	Backup               *backup.SetupObservation      `json:"backup,omitempty"`
	Diagnostic           *install.DiagnosticResult     `json:"diagnostic,omitempty"`
	Version              int                           `json:"version"`
	State                string                        `json:"state"`
	Code                 string                        `json:"code"`
	Next                 string                        `json:"next,omitempty"`
	Identity             *protocol.Identity            `json:"identity,omitempty"`
}
type Controller interface {
	Apply(context.Context, config.ServerConfig, Artifact, ControlRequest) (ControlResult, error)
}
type SSHController struct{}

func writeControlFrame(out io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > maxControlFrame {
		return fmt.Errorf("control frame exceeds limit")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err = out.Write(header[:]); err != nil {
		return err
	}
	_, err = out.Write(raw)
	return err
}
func readControlFrame(in io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return fmt.Errorf("control frame header unavailable")
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxControlFrame {
		return fmt.Errorf("invalid control frame size")
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(in, raw); err != nil {
		return fmt.Errorf("truncated control frame")
	}
	if err := safefs.DecodeJSON(raw, v, maxControlFrame); err != nil {
		return fmt.Errorf("invalid control frame fields")
	}
	return nil
}

func (SSHController) Apply(ctx context.Context, s config.ServerConfig, a Artifact, r ControlRequest) (ControlResult, error) {
	if r.InteractiveLink {
		return ControlResult{}, fmt.Errorf("private linking display required")
	}
	return sshWorkerControl(ctx, s, a, r, nil)
}
func (SSHController) ApplyInteractive(ctx context.Context, s config.ServerConfig, a Artifact, r ControlRequest, display io.Writer) (ControlResult, error) {
	if display == nil {
		return ControlResult{}, fmt.Errorf("private linking display required")
	}
	r.InteractiveLink = true
	return sshWorkerControl(ctx, s, a, r, display)
}
func sshWorkerControl(ctx context.Context, s config.ServerConfig, a Artifact, r ControlRequest, display io.Writer) (ControlResult, error) {
	var result ControlResult
	if err := VerifyArtifact(a, r.Facts.Arch, true); err != nil {
		return result, err
	}
	f, err := os.Open(a.File)
	if err != nil {
		return result, fmt.Errorf("helper unavailable")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<20 {
		f.Close()
		return result, fmt.Errorf("helper changed before bootstrap")
	}
	helper, err := io.ReadAll(io.LimitReader(f, (128<<20)+1))
	f.Close()
	if err != nil || len(helper) > 128<<20 || fmt.Sprintf("%x", sha256.Sum256(helper)) != a.SHA256 {
		return result, fmt.Errorf("helper changed before bootstrap")
	}
	r.HelperSHA256 = a.SHA256
	var native *os.File
	var nativeSize int64
	if r.LocalSignal != nil {
		if r.Desired.Notification == nil {
			return result, fmt.Errorf("unexpected Signal artifact")
		}
		declared, ok := r.Desired.Notification.Providers["signal"]
		if !ok || declared.Executable != "" {
			return result, fmt.Errorf("unexpected Signal artifact")
		}
		if err = VerifySignalArtifact(*r.LocalSignal, r.Facts.Arch); err != nil {
			return result, err
		}
		native, err = os.Open(r.LocalSignal.File)
		if err != nil {
			return result, fmt.Errorf("Signal artifact unavailable")
		}
		defer native.Close()
		info, e := native.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 512<<20 {
			return result, fmt.Errorf("Signal artifact changed before transfer")
		}
		nativeSize = info.Size()
		r.SignalSHA256 = r.LocalSignal.SHA256
	}
	var ledArchive []byte
	if r.LocalLED != nil {
		if r.Desired.Notification == nil || !r.Desired.Notification.StatusLEDs.Enabled || r.Desired.Notification.StatusLEDs.Daemon.Mode != "managed" {
			return result, fmt.Errorf("unexpected LED artifact")
		}
		verified, e := ResolveLEDArtifact(r.Desired.Notification.StatusLEDs.Daemon, r.LocalLED)
		if e != nil {
			return result, e
		}
		if *verified != *r.LocalLED {
			return result, fmt.Errorf("LED artifact differs from declaration")
		}
		file, e := os.Open(verified.File)
		if e != nil {
			return result, e
		}
		ledArchive, e = io.ReadAll(io.LimitReader(file, (8<<20)+1))
		file.Close()
		if e != nil || len(ledArchive) > 8<<20 || inputDigest(ledArchive) != verified.SHA256 {
			return result, fmt.Errorf("LED artifact changed before transfer")
		}
		r.LEDSHA256 = verified.SHA256
	}
	command := "__worker_control"
	if display != nil {
		command = "__worker_control_stream"
	}
	var request bytes.Buffer
	if err = writeControlFrame(&request, r); err != nil {
		return result, err
	}
	script := fmt.Sprintf(`import os,sys,tempfile,hashlib,subprocess
root=tempfile.mkdtemp(prefix="deploy-worker-control-")
helper=os.path.join(root,"deploy")
native=os.path.join(root,"signal-cli")
led_artifact=os.path.join(root,"led-daemon.pyz")
def receive(path,size,expected):
 h=hashlib.sha256()
 with open(path,"xb") as out:
  os.chmod(path,0o700)
  while size:
   data=os.read(0,min(size,65536))
   if not data: raise RuntimeError("truncated artifact")
   out.write(data);h.update(data);size-=len(data)
  out.flush();os.fsync(out.fileno())
 if h.hexdigest()!=expected: raise RuntimeError("artifact digest mismatch")
try:
 receive(helper,%d,%q)
 env=os.environ.copy()
 env.pop("DEPLOY_CONTROL_SIGNAL_ARTIFACT",None)
 if %d:
  receive(native,%d,%q)
  env["DEPLOY_CONTROL_SIGNAL_ARTIFACT"]=native
 env.pop("DEPLOY_CONTROL_LED_ARTIFACT",None)
 if %d:
  receive(led_artifact,%d,%q)
  env["DEPLOY_CONTROL_LED_ARTIFACT"]=led_artifact
 status=subprocess.call([helper,%q],env=env)
finally:
 for path in [led_artifact,native,helper]:
  try: os.unlink(path)
  except FileNotFoundError: pass
 os.rmdir(root)
sys.exit(status)
`, len(helper), a.SHA256, nativeSize, nativeSize, r.SignalSHA256, len(ledArchive), len(ledArchive), r.LEDSHA256, command)
	cmd := transport.WorkerAdminShell(s, "python3 -I -c "+transport.ShellQuote(script))
	readers := []io.Reader{bytes.NewReader(helper)}
	if native != nil {
		readers = append(readers, native)
	}
	if len(ledArchive) > 0 {
		readers = append(readers, bytes.NewReader(ledArchive))
	}
	readers = append(readers, bytes.NewReader(request.Bytes()))
	cmd.Stdin = io.MultiReader(readers...)
	var out, errs process.Buffer
	out.Limit = maxControlFrame + 4
	errs.Limit = 8192
	cmd.Stdout = &out
	events := controlEventReceiver{display: display}
	if display != nil {
		cmd.Stdout = &events
	}
	cmd.Stderr = &errs
	if err = process.RunContext(ctx, cmd, 20*time.Minute, 5*time.Second); err != nil {
		return result, fmt.Errorf("worker control transport interrupted; retry the same declaration to resume")
	}
	if display != nil {
		result, err = events.finish()
	} else {
		err = readControlFrame(bytes.NewReader(out.Bytes()), &result)
	}
	if err != nil {
		return result, err
	}
	if result.Version != 1 {
		return result, fmt.Errorf("unsupported control response")
	}
	if result.Code != "ok" && ((r.Action != "status" && r.Action != "plan" && r.Action != "import") || result.Code == "operation_failed" || result.Code == "invalid_request") {
		return result, controlResponseError{Result: result}
	}
	return result, nil
}

// Preserve a validated remote result through CLI handlers that return errors
// before printing. JSON callers need its lifecycle code and resume guidance.
type controlResponseError struct{ Result ControlResult }

func (e controlResponseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Result.Code, e.Result.Next)
}

// ControlMain is internal, versioned and stdin-only. Error responses never echo
// request frames, credential contents or subprocess stderr.
func ControlMain(ctx context.Context, in io.Reader, out io.Writer) error {
	var request ControlRequest
	result := ControlResult{Version: 1, State: "needs_action", Code: "invalid_request"}
	if err := readControlFrame(in, &request); err != nil {
		result.Next = err.Error()
		return writeControlFrame(out, result)
	}
	result, err := applyControl(ctx, request)
	if err != nil {
		result = controlFailure(err)
	}
	return writeControlFrame(out, result)
}

func applyControl(ctx context.Context, r ControlRequest) (ControlResult, error) {
	return applyControlWithLinkOutput(ctx, r, nil)
}
func applyControlWithLinkOutput(ctx context.Context, r ControlRequest, linkOutput io.Writer) (result ControlResult, err error) {
	result = ControlResult{Version: 1}
	defer func() {
		var needed install.FreshSignalResumeRequired
		var described signalServiceResumeError
		if errors.As(err, &needed) && !errors.As(err, &described) {
			err = signalServiceResumeError{Server: r.Server, Cause: err}
		}
	}()
	if r.InteractiveLink && linkOutput == nil {
		return result, fmt.Errorf("private linking channel unavailable")
	}
	if r.SignalSHA256 != "" || r.InteractiveLink || r.SignalReview != "" {
		if r.Desired.Notification == nil {
			return result, fmt.Errorf("unexpected Signal control input")
		}
		if _, ok := r.Desired.Notification.Providers["signal"]; !ok {
			return result, fmt.Errorf("unexpected Signal control input")
		}
		if r.SignalSHA256 != "" && !validDigest(r.SignalSHA256) {
			return result, fmt.Errorf("invalid Signal artifact digest")
		}
		if r.SignalReview != "" && (!safefs.ValidID(r.SignalReview, "n") || (r.Action != "apply" && r.Action != "resume")) {
			return result, fmt.Errorf("invalid Signal link review")
		}
	}
	if r.Version != 1 || (r.Worker != "notification-worker" && r.Worker != "backup-worker") || (r.Action != "apply" && r.Action != "resume" && r.Action != "status" && r.Action != "plan" && r.Action != "import" && r.Action != "adopt" && r.Action != "uninstall" && r.Action != "test" && r.Action != "activate" && r.Action != "run" && !isQualificationControl(r.Action)) || !config.ValidInventoryName(r.Server) || !validDigest(r.HelperSHA256) || (r.Worker == "backup-worker" && r.Action != "apply" && r.Action != "resume" && r.Action != "status" && r.Action != "plan" && r.Action != "import" && r.Action != "activate" && r.Action != "run" && !isQualificationControl(r.Action)) || (r.Worker == "notification-worker" && (r.Action == "activate" || r.Action == "run")) {
		return result, fmt.Errorf("unsupported worker control request")
	}
	if !isQualificationControl(r.Action) && r.Observation != nil {
		return result, fmt.Errorf("unexpected qualification observation")
	}
	if r.DiagnosticProvider != "" && (r.Action != "test" || r.Worker != "notification-worker" || !config.ValidInventoryName(r.DiagnosticProvider)) {
		return result, fmt.Errorf("unexpected diagnostic provider selection")
	}
	if err := validateSourceTargets(r); err != nil {
		return result, err
	}
	if r.Action != "apply" && r.Action != "resume" && len(r.Inputs) != 0 {
		return result, fmt.Errorf("read/removal operations do not accept credential inputs")
	}
	if r.DesiredScope != 0 && (r.DesiredScope != 1 || r.Worker != "notification-worker") {
		return result, fmt.Errorf("unsupported desired policy scope")
	}
	hash, err := declarationFingerprint(r.Desired, r.DesiredScope)
	if err != nil || hash != r.DesiredHash {
		return result, fmt.Errorf("desired policy pin mismatch")
	}
	u, err := user.Current()
	if err != nil {
		return result, fmt.Errorf("target identity unavailable")
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil || uid <= 0 || uid != os.Getuid() || uid != r.Facts.UID || u.Username != r.Facts.User || u.HomeDir != r.Facts.Home || runtime.GOOS != r.Facts.OS || runtime.GOARCH != r.Facts.Arch {
		return result, fmt.Errorf("target identity changed since inspection")
	}
	if os.Getenv("XDG_CONFIG_HOME") != r.Facts.ConfigHome || os.Getenv("XDG_STATE_HOME") != r.Facts.StateHome || os.Getenv("XDG_DATA_HOME") != r.Facts.DataHome {
		return result, fmt.Errorf("target XDG paths changed since inspection")
	}
	disabling := r.Worker == "notification-worker" && r.Desired.Notification != nil && !config.WorkerEnabled(r.Desired.Notification.Enabled) || r.Worker == "backup-worker" && r.Desired.Backup != nil && !config.WorkerEnabled(r.Desired.Backup.Enabled)
	if !r.Facts.UserManager || (r.Action == "apply" || r.Action == "resume") && !disabling && !r.Facts.Linger {
		return result, fmt.Errorf("target user manager and lingering are required")
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	if err = VerifyArtifact(Artifact{executable, r.HelperSHA256}, r.Facts.Arch, true); err != nil {
		return result, err
	}
	if r.Worker == "backup-worker" {
		if r.Action == "import" {
			return importBackupControl(ctx, r)
		}
		if r.Action == "apply" || r.Action == "resume" {
			return activityBackupControl(ctx, r, backup.ReconcileDeclaredActivity)
		}
		if r.Action == "run" {
			return requestBackupRunControl(ctx, r, func(ctx context.Context, home, desired, helper string) (backup.RunRequestResult, error) {
				return backup.RequestDeclaredBackup(ctx, home, desired, helper, r.SourceTargets...)
			})
		}
		if r.Action == "plan" {
			return planBackupControl(ctx, r, backup.InspectSetupPolicy)
		}
		if isQualificationControl(r.Action) || r.Action == "activate" {
			return operateBackupControl(ctx, r, func(ctx context.Context, home, desired, helper, action string, observation install.QualificationObservation) (install.QualificationSession, error) {
				return backup.QualifyDeclaredWorker(ctx, home, desired, helper, action, observation, r.SourceTargets...)
			}, func(ctx context.Context, home, desired, helper string) (backup.ActivationResult, error) {
				return backup.ActivateDeclaredWorker(ctx, home, desired, helper, r.SourceTargets...)
			})
		}
		return inspectBackupControl(ctx, r)
	}
	if r.Desired.Notification == nil {
		return result, fmt.Errorf("notification declaration is missing")
	}
	if (r.Action == "apply" || r.Action == "resume") && config.WorkerEnabled(r.Desired.Notification.Enabled) {
		if err = directNotificationSupport(r.Desired); err != nil {
			return result, err
		}
	}
	paths, err := ResolvePaths(r.Worker, r.Desired.Notification.Paths, r.Facts)
	if err != nil {
		return result, err
	}
	if r.Action == "import" {
		return importNotificationControl(ctx, r, paths, install.InspectPolicyReview)
	}
	if (r.Action == "apply" || r.Action == "resume") && config.WorkerEnabled(r.Desired.Notification.Enabled) {
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultDeliveryDerivedFreshness(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultDeliveryDeclaration(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultDeliveryProviderEnrollment(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultDeliveryDestination(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultDeliveryProviders(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProviders(ctx, r, paths, executable, defaultCommandDeliveryLimits(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryLimits(ctx, r, paths, executable, defaultDeliveryLimits(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryProducer(ctx, r, paths, executable, defaultDeliveryProducer(executable)); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileDeliveryRoutes(ctx, r, paths, executable, defaultDeliveryRoutes(executable)); handled || e != nil {
			return policy, e
		}
	}
	if r.Action == "qualify-begin" || r.Action == "apply" || r.Action == "resume" {
		commandRenewal, commandHandled, commandErr := reconcileCommandRequalification(ctx, r, paths, defaultCommandRequalification(executable))
		if commandErr != nil {
			return commandRenewal, commandErr
		}
		if commandHandled && r.Action != "qualify-begin" {
			return commandRenewal, nil
		}
		renewal, handled, e := reconcileRequalification(ctx, r, paths, defaultRequalification(executable))
		if e != nil {
			return renewal, e
		}
		if handled && r.Action != "qualify-begin" {
			return renewal, nil
		}
	}
	if (r.Action == "apply" || r.Action == "resume") && config.WorkerEnabled(r.Desired.Notification.Enabled) {
		if policy, handled, e := reconcileSharedProviders(ctx, r, paths, executable, defaultSharedProviders()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedRoutes(ctx, r, paths, executable, defaultSharedRoutes()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedProducerPolicy(ctx, r, paths, executable, defaultSharedProducerPolicy()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedCommandActivity(ctx, r, paths, executable, defaultSharedCommandActivity()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedCommandPolicy(ctx, r, paths, executable, defaultSharedCommandPolicy()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedCommands(ctx, r, paths, executable, defaultSharedCommands()); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedPolicy(ctx, r, paths, executable, defaultSharedReportRoute(), "report-route"); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedPolicy(ctx, r, paths, executable, defaultSharedDerivedFreshness(), "derived-freshness"); handled || e != nil {
			return policy, e
		}
		if policy, handled, e := reconcileSharedFreshness(ctx, r, paths, executable, defaultSharedFreshness()); handled || e != nil {
			return policy, e
		}
		if limits, handled, e := reconcileSharedLimits(ctx, r, paths, executable, defaultSharedLimits()); handled || e != nil {
			return limits, e
		}
		if output, handled, e := reconcileDeliveryLEDOutput(ctx, r, paths, defaultDeliveryOutput(executable)); handled || e != nil {
			return output, e
		}
		if activity, handled, e := reconcileCommandActivity(ctx, r, paths, defaultCommandActivity()); handled || e != nil {
			return activity, e
		}
		if output, handled, e := reconcileEnrolledLEDOutput(ctx, r, paths, executable, defaultNotificationRemapping(), notificationLEDLifecycle{install.InspectInstallation, install.SetLEDOutput}); handled || e != nil {
			return output, e
		}
		if mapped, handled, e := reconcileDeclaredLEDMappings(ctx, r, paths, executable, defaultNotificationRemapping()); handled || e != nil {
			return mapped, e
		}
	}
	if err = resumeCompletedSignalChild(ctx, r, paths, executable, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), defaultSignalSetupLifecycle()); err != nil {
		return result, err
	}
	if r.Action == "adopt" || r.Action == "apply" || r.Action == "resume" {
		adopted, handled, e := reconcileNotificationAdoption(ctx, r, paths, notificationAdoptionLifecycle{review: install.InspectPolicyReview})
		if handled || e != nil {
			if e == nil && r.Action != "adopt" {
				return continueActivityEnrollment(ctx, r, paths, executable, adopted)
			}
			return adopted, e
		}
	}
	if r.Action == "apply" || r.Action == "resume" {
		draft, handled, e := reconcileNotificationCommandDraft(ctx, r, paths, executable, notificationLimitsLifecycle{inspect: install.InspectInstallation})
		if handled || e != nil {
			if e == nil {
				return continueActivityEnrollment(ctx, r, paths, executable, draft)
			}
			return draft, e
		}
		release, handled, e := reconcileNotificationRelease(ctx, r, paths, executable, notificationReleaseLifecycle{install.InspectInstallation, install.UpdateBootstrapVerification})
		if handled || e != nil {
			if e == nil {
				return continueActivityEnrollment(ctx, r, paths, executable, release)
			}
			return release, e
		}
		limits, handled, e := reconcileNotificationLimits(ctx, r, paths, executable, notificationLimitsLifecycle{inspect: install.InspectInstallation, migrate: install.MigrateBootstrapLimits})
		if handled || e != nil {
			if e == nil {
				return continueActivityEnrollment(ctx, r, paths, executable, limits)
			}
			return limits, e
		}
		routes, handled, e := reconcileNotificationRoutes(ctx, r, paths, executable, notificationLimitsLifecycle{inspect: install.InspectInstallation, migrate: install.MigrateBootstrapRoutes})
		if handled || e != nil {
			if e == nil {
				return continueActivityEnrollment(ctx, r, paths, executable, routes)
			}
			return routes, e
		}
		providers, handled, e := reconcileNotificationProviders(ctx, r, paths, executable, notificationLimitsLifecycle{inspect: install.InspectInstallation, migrate: install.MigrateBootstrapProviders})
		if handled || e != nil {
			if e == nil {
				return continueActivityEnrollment(ctx, r, paths, executable, providers)
			}
			return providers, e
		}
		activity, handled, e := reconcileNotificationActivity(ctx, r, paths, notificationActivityLifecycle{install.InspectRetainedInstallation, install.UninstallVerification, install.RestoreBootstrapVerification, install.SetLEDOutput})
		if handled || e != nil {
			if e == nil && activity.State == "waiting_for_producers" {
				return continueActivityEnrollment(ctx, r, paths, executable, activity)
			}
			return activity, e
		}
		prerequisite, e := prepareManagedLED(ctx, r, paths, executable, os.Getenv("DEPLOY_CONTROL_LED_ARTIFACT"), ledSetupLifecycle{ledhost.Discover, ledhost.ReadReceipt})
		if e != nil {
			return result, e
		}
		if prerequisite != nil {
			return ControlResult{Version: 1, State: "needs_action", Code: "led_host_prerequisite", LEDPrerequisite: prerequisite, Next: "review the staged LED host prerequisite on " + r.Server + ", apply its displayed administrator command, then run deploy notification-worker resume " + r.Server}, nil
		}
		if changed, handled, e := reconcileDeclaredLEDOutput(ctx, r, paths, notificationLEDLifecycle{install.InspectInstallation, install.SetLEDOutput}); handled || e != nil {
			return changed, e
		}
	}
	if isQualificationControl(r.Action) {
		return qualifyNotificationControl(ctx, r, paths, defaultQualificationLifecycle())
	}
	if r.Action == "test" {
		return testNotificationControl(ctx, r, paths, func(ctx context.Context, root, expected string) (install.DiagnosticResult, error) {
			return install.TestDeliveryOnceFor(ctx, root, expected, r.DiagnosticProvider)
		})
	}
	if r.Action == "uninstall" {
		removed, e := uninstallNotificationControl(ctx, r, paths, install.InspectRetainedInstallation, install.UninstallVerification)
		if e != nil {
			return removed, e
		}
		return finishNotificationHostRemoval(ctx, r, paths, executable, removed, ledRemovalLifecycle{ledhost.ReadReceipt, ledhost.ReadRemovalReceipt})
	}
	if r.Action == "plan" {
		return planNotificationControl(ctx, r, paths, install.InspectRetainedInstallation)
	}
	if r.Action == "status" {
		return inspectNotificationControl(ctx, r, paths, install.InspectInstallation, install.InspectQualification)
	}
	r.ledEndpoint, err = inspectDeclaredLED(ctx, r, led.InspectEndpoint)
	if err != nil {
		return result, err
	}
	lifecycle := notificationLifecycle{install.EnsureVerification, install.InspectVerification}
	if _, ok := r.Desired.Notification.Providers["signal"]; ok {
		service, _, secrets, err := prepareDeclaredSignal(ctx, r, paths, executable, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), linkOutput, defaultSignalSetupLifecycle())
		if errors.Is(err, errSignalLinkRequired) {
			return ControlResult{Version: 1, State: "needs_action", Code: "signal_link_required", Next: "run deploy notification-worker resume " + r.Server + " in a terminal to link the reserved account"}, nil
		}
		var review signalLinkReviewRequired
		if errors.As(err, &review) {
			return ControlResult{Version: 1, State: "needs_action", Code: "signal_link_review_required", SignalReview: review.Operation, Next: "inspect linked devices in Signal, then run deploy notification-worker resume " + r.Server + " in a terminal to review the retained attempt"}, nil
		}
		if err != nil {
			return result, err
		}
		return applyNotificationBootstrapWithSignal(ctx, r, paths, executable, lifecycle, &service, secrets)
	}
	return applyNotificationBootstrap(ctx, r, paths, executable, lifecycle)
}

type notificationLifecycle struct {
	ensure  func(context.Context, install.ManagedSpec) (install.ManagedManifest, error)
	inspect func(context.Context, install.ManagedSpec) (install.ManagedManifest, bool, error)
}

func applyNotificationBootstrap(ctx context.Context, r ControlRequest, paths ResolvedPaths, executable string, lifecycle notificationLifecycle) (ControlResult, error) {
	return applyNotificationBootstrapWithSignal(ctx, r, paths, executable, lifecycle, nil, nil)
}
func applyNotificationBootstrapWithSignal(ctx context.Context, r ControlRequest, paths ResolvedPaths, executable string, lifecycle notificationLifecycle, signal *install.FreshSignalManifest, signalSecrets map[string][]byte) (ControlResult, error) {
	result := ControlResult{Version: 1}
	p, telegram := r.Desired.Notification.Providers["telegram"]
	secrets := map[string][]byte{}
	for k, v := range signalSecrets {
		secrets[k] = v
	}
	var err error
	if err = readDeclaredTelegramInputs(r, paths, secrets); err != nil {
		return result, err
	}
	for field := range r.Inputs {
		if telegram && (field == "token" || field == "chat_id") {
			continue
		}
		if signal != nil && (field == "signal_account" || field == "signal_recipient") {
			continue
		}
		return result, fmt.Errorf("unexpected credential field")
	}
	var input install.RuntimeInput
	extraInputs := map[string]string{}
	if signal != nil {
		input, err = CompileFreshSignalNotification(r.Desired, paths, r.Facts.Home, secrets, *signal)
		extraInputs["signal_account"] = signal.Spec.AccountFile
		extraInputs["signal_recipient"] = signal.Spec.RecipientFile
	} else {
		input, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, secrets)
	}
	if err != nil {
		return result, err
	}
	if input.LEDSocket != "" {
		if r.ledEndpoint == nil || r.ledEndpoint.Socket != input.LEDSocket {
			return result, fmt.Errorf("LED endpoint preflight required")
		}
		input.LEDEndpoint = r.ledEndpoint
	}
	c, err := input.Resolve()
	if err != nil {
		return result, err
	}
	// The directory is the outer authority; policy conflicts are checked under its
	// lock before the child is allowed to create any runtime or credential files.
	d, err := ensureConfigDirectory(paths.Control)
	if err != nil {
		return result, err
	}
	defer d.Close()
	var previous OperationJournal
	if readErr := d.ReadJSON("operation.json", &previous); readErr == nil && (enrolledNotificationJournal(previous) || len(previous.Children) == 1 && previous.Children[0].Name == "notification-delivery" || commandJournal(previous)) {
		selectedProviders := protocol.Hash(input.Providers)
		if err = retainBootstrapLimitsInput(d, previous, &input); err != nil {
			return result, err
		}
		if protocol.Hash(input.Providers) != selectedProviders {
			for field, path := range map[string]string{"token": input.Providers["telegram"].TokenFile, "chat_id": input.Providers["telegram"].ChatFile} {
				secrets[field], err = ReadCredentialInput(path)
				if err != nil {
					return result, err
				}
			}
		}
		step := &notificationBootstrapStep{paths: paths, input: input, secrets: secrets, extraInputs: extraInputs}
		if err = step.verifyFiles(); err != nil {
			return result, err
		}
		input = step.input
		if commandJournal(previous) {
			prior, e := commandPredecessor(d, previous)
			if e != nil {
				return result, e
			}
			if e = verifyAdmittedConsumerInput(r, paths, d, prior, input); e != nil {
				return result, e
			}
			return advanceNotificationCommands(ctx, r, paths, d, previous, result, defaultNotificationCommands())
		}
		if previous.Children[0].Name == "notification-delivery" {
			if err = verifyAdmittedConsumerInput(r, paths, d, previous, input); err != nil {
				return result, err
			}
			return advanceNotificationDelivery(ctx, r, paths, d, previous, result, defaultNotificationDelivery())
		}
		return reconcileReservedBackup(ctx, r, paths, executable, d, previous, input, result)
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return result, readErr
	}
	spec := install.ManagedSpec{Version: 1, Root: paths.Installation, State: paths.State, UnitDirectory: paths.UnitDirectory, RuntimeFile: paths.Runtime, BinaryFile: executable, BinarySHA256: r.HelperSHA256, DeniedPaths: paths.DeniedPaths}
	if signal != nil {
		spec.FreshSignal = &install.FreshSignalReference{Root: signal.Spec.Root, ManifestHash: signal.ManifestHash}
	}
	step := &notificationBootstrapStep{desired: &r.Desired, desiredHash: r.DesiredHash, desiredScope: r.DesiredScope, paths: paths, spec: spec, input: input, resolved: c, secrets: secrets, provider: p, lifecycle: lifecycle, extraInputs: extraInputs}
	pins := OperationPins{DesiredScope: r.DesiredScope, Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}, Artifacts: map[string]string{"helper": r.HelperSHA256}}
	if r.Desired.Notification.StatusLEDs.Enabled && r.Desired.Notification.StatusLEDs.Daemon.Mode == "managed" {
		if !validDigest(r.LEDSHA256) {
			return result, fmt.Errorf("managed LED artifact pin missing")
		}
		pins.Artifacts["led-daemon"] = r.LEDSHA256
	}
	record, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	if err != nil {
		return result, err
	}
	result.State = "waiting_for_producers"
	result.Code = "ok"
	result.Identity = &step.manifest.Identity
	result.Next = "gateway verification service installed; real producer enrollment and delivery qualification remain required"
	if r.Desired.Notification.StatusLEDs.Enabled && len(r.Desired.Notification.StatusLEDs.Bindings) != 0 {
		result.Next += "; declared LED bindings wait for the real backup producer; only aggregate output is installed so far"
	}
	return reconcileReservedBackup(ctx, r, paths, executable, d, record, input, result)
}

type notificationBootstrapStep struct {
	desired      *config.DesiredWorkers
	desiredHash  string
	desiredScope int
	extraInputs  map[string]string
	paths        ResolvedPaths
	spec         install.ManagedSpec
	input        install.RuntimeInput
	resolved     install.Runtime
	secrets      map[string][]byte
	provider     config.ProviderDeclaration
	lifecycle    notificationLifecycle
	manifest     install.ManagedManifest
}

func (s *notificationBootstrapStep) Name() string { return "notification-bootstrap" }
func (s *notificationBootstrapStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	if d, err := safefs.Open(s.paths.Installation); err == nil {
		d.Close()
		if err = s.verifyInputs(op); err != nil {
			return "", false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	m, done, err := s.lifecycle.inspect(ctx, s.spec)
	if err != nil || !done {
		return "", done, err
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *notificationBootstrapStep) Run(ctx context.Context, op string) error {
	if s.desired != nil {
		d, err := safefs.Open(s.paths.Control)
		if err != nil {
			return err
		}
		err = retainDeclaration(d, *s.desired, s.desiredHash, s.desiredScope)
		d.Close()
		if err != nil {
			return err
		}
	}
	if err := s.pinInputs(op, true); err != nil {
		return err
	}
	for _, path := range []string{s.paths.Config, filepath.Dir(s.paths.Runtime), filepath.Dir(s.paths.State), filepath.Dir(s.paths.Installation), s.paths.UnitDirectory} {
		d, err := ensureConfigDirectory(path)
		if err != nil {
			return err
		}
		d.Close()
	}
	for field, path := range map[string]string{"token": s.input.Providers["telegram"].TokenFile, "chat_id": s.input.Providers["telegram"].ChatFile} {
		local := s.provider.TokenInput
		if field == "chat_id" {
			local = s.provider.ChatIDInput
		}
		if local == "" {
			continue
		}
		if err := publishPrivateInput(path, s.secrets[field]); err != nil {
			return err
		}
	}
	raw, err := s.input.Bytes()
	if err != nil {
		return err
	}
	if err = publishPrivateInput(s.paths.Runtime, raw); err != nil {
		return err
	}
	if d, e := safefs.Open(s.paths.Installation); e == nil {
		d.Close()
		if err = s.verifyInputs(op); err != nil {
			return err
		}
	} else if errors.Is(e, os.ErrNotExist) {
		if _, err = worker.BootstrapGateway(ctx, s.paths.State, op, s.resolved.Worker); err != nil {
			return err
		}
	} else {
		return e
	}
	_, err = s.lifecycle.ensure(ctx, s.spec)
	return err
}

func publishPrivateInput(path string, body []byte) error {
	d, err := ensureConfigDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	existing, err := safefs.ReadFile(d, filepath.Base(path), maxControlFrame)
	if err == nil {
		info, readErr := readPrivateGeneratedInput(path, len(body))
		if readErr != nil || !bytes.Equal(info, body) || !bytes.Equal(existing, body) {
			return fmt.Errorf("private input changed; explicit rotation or migration required")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("private input destination is unsafe")
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id
	f, err := d.Open(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(d.File().Fd()), tmp, 0)
	_, err = f.Write(body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.Rename(tmp, d, filepath.Base(path))
}
func readPrivateGeneratedInput(path string, size int) ([]byte, error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0600 || info.Size() != int64(size) {
		return nil, fmt.Errorf("private generated file permissions or size changed")
	}
	var stat unix.Stat_t
	if err = unix.Fstat(int(f.Fd()), &stat); err != nil || stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("private generated file ownership changed")
	}
	return io.ReadAll(io.LimitReader(f, maxControlFrame+1))
}

func (s *notificationBootstrapStep) verifyFiles() error {
	if err := s.pinInputs("", false); err != nil {
		return err
	}
	raw, err := s.inputBytes()
	if err != nil {
		return err
	}
	inputs := map[string][]byte{s.paths.Runtime: raw}
	if p, ok := s.input.Providers["telegram"]; ok {
		inputs[p.TokenFile] = s.secrets["token"]
		inputs[p.ChatFile] = s.secrets["chat_id"]
	}
	for field, path := range s.extraInputs {
		inputs[path] = s.secrets[field]
	}
	for path, want := range inputs {
		got, e := readPrivateGeneratedInput(path, len(want))
		if e != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("private input changed; credential rotation or runtime migration required")
		}
	}
	return nil
}
func (s *notificationBootstrapStep) verifyInputs(op string) error {
	if err := s.pinInputs(op, false); err != nil {
		return err
	}
	if err := s.verifyFiles(); err != nil {
		return err
	}
	d, err := safefs.Open(s.paths.State)
	if err != nil {
		return err
	}
	defer d.Close()
	var b worker.GatewayBootstrap
	if err = d.ReadJSON("bootstrap.json", &b); err != nil {
		return err
	}
	if b.Operation != op || b.State != s.paths.State || protocol.Hash(b.Config) != protocol.Hash(s.resolved.Worker) {
		return fmt.Errorf("gateway bootstrap authority differs; explicit import required")
	}
	return nil
}

func inspectNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, inspect func(context.Context, string) (install.InstallationObservation, error), qualification func(context.Context, string, string) (install.QualificationSummary, error)) (ControlResult, error) {
	if pending, found, err := inspectDeclaredLEDProgress(ctx, r, paths, ledhost.ReadReceipt); found || err != nil {
		return pending, err
	}
	if r.Desired.Notification != nil {
		if pending, found, err := inspectDeclaredSignalProgress(ctx, r, paths); found || err != nil {
			return pending, err
		}
	}
	result := ControlResult{Version: 1, State: "needs_action", Code: "admission_unverified"}
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		result.Next = "no declarative coordinator record; inspect or import the existing installation"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var record OperationJournal
	if err = d.ReadJSON("operation.json", &record); err != nil {
		return result, err
	}
	if err = validateOperationJournal(record); err != nil {
		return result, err
	}
	if record.Pins.UID != r.Facts.UID || record.Pins.Home != r.Facts.Home || record.Pins.Installations["notification-worker"] != paths.Installation || record.Pins.Installations["state"] != paths.State || record.Pins.Installations["runtime"] != paths.Runtime {
		return result, fmt.Errorf("installation paths or target identity changed; migration required")
	}
	if digest := record.Pins.Artifacts["led-daemon"]; digest != "" && record.Pins.Action != "uninstall" {
		host, e := ledhost.ReadReceipt(r.Facts.UID)
		if e != nil || host.Spec.ArtifactSHA256 != digest {
			return result, fmt.Errorf("managed LED host authority missing or changed; inspect the retained host prerequisite")
		}
	}
	observation, err := inspect(ctx, paths.Installation)
	if err != nil {
		return result, err
	}
	result.Identity = &observation.Manifest.Identity
	result.LEDOutput = observation.Manifest.LEDOutput
	if !record.Complete || observation.Phase != "complete" {
		result.LEDOutputPending = len(record.Children) == 1 && (record.Children[0].Name == "notification-led-output" || enrolledOutputJournal(record) || deliveryOutputJournal(record))
		result.State = "resumable"
		result.Code = "pending_operation"
		result.Next = "repeat the same declaration to resume the retained operation"
		if record.Pins.Action == "uninstall" {
			result.Code = "pending_removal"
			result.Next = "deploy notification-worker uninstall " + r.Server
			if len(record.Children) == 2 && record.Children[0].Name == "notification-remove-led-output" && !record.Children[0].Complete {
				result.State, result.Code, result.LEDOutputPending = "cleanup_pending", "led_output_pending", true
			}
		}
		return result, nil
	}
	if receipt, valid := canonicalNotificationReceipt(record); !valid || receipt != protocol.Hash(observation.Manifest) {
		return result, fmt.Errorf("canonical installation changed outside the recorded coordinator operation")
	}
	if deliveryProviderJournal(record) {
		if e := verifyDeliveryProviderReceipt(d, record, observation.Manifest, nil); e != nil {
			return result, e
		}
	}
	if commandLimitsJournal(record) {
		if _, err = retainedCommandReceiver(ctx, d, record, defaultNotificationCommands()); err != nil {
			return result, err
		}
	}
	if requalificationJournal(record) {
		_, v, e := requalificationPredecessor(d, record)
		if e != nil {
			return result, e
		}
		if e = validateRequalificationManifest(v.Before, observation.Manifest); e != nil {
			return result, e
		}
		if e = verifyMappingScope(d, record, nil); e != nil {
			return result, e
		}
	}
	if commandJournal(record) {
		if _, err = commandPredecessor(d, record); err != nil {
			return result, err
		}
		version := 1
		if observation.Manifest.Commands == nil || (observation.Manifest.Commands.Mode != "read" && observation.Manifest.Commands.Mode != "all") {
			return result, fmt.Errorf("command receiver lost its admitted mode")
		}
		if observation.Manifest.Commands.Mode == "all" {
			version = 2
		}
		receiver := backupCommandReceiverStep{version: version, directory: d, lifecycle: defaultNotificationCommands()}
		pin, complete, e := receiver.Inspect(ctx, record.Children[0].Operation)
		if e != nil {
			return result, e
		}
		if !complete || pin != record.Children[0].Receipt || receiver.request.Identity != observation.Manifest.Identity {
			return result, fmt.Errorf("command receiver authority changed")
		}
	}
	if deliveryOutputJournal(record) {
		if err = verifyDeliveryOutputReceipt(d, record, observation.Manifest, backup.InspectVerificationNotificationScope); err != nil {
			return result, err
		}
		if admittedCommandHistory(record) {
			if _, err = retainedCommandReceiver(ctx, d, record, defaultNotificationCommands()); err != nil {
				return result, err
			}
		}
	}
	if commandPauseJournal(record) || commandDeliveryJournal(record) {
		if err = verifyCommandRenewalReceipt(ctx, d, record, observation.Manifest, defaultCommandRequalification("")); err != nil {
			return result, err
		}
	}
	if commandActivityJournal(record) {
		if err = verifyCommandActivityReceipt(ctx, d, record, observation.Manifest, defaultCommandActivity()); err != nil {
			return result, err
		}
	}
	if err = verifyRemovalLEDReceipt(ctx, d, record, paths.Installation, inspect); err != nil {
		return result, err
	}
	if err = verifyActivityReceipt(d, record, observation.Manifest); err != nil {
		return result, err
	}
	if err = verifyReleaseReceipt(d, record, observation.Manifest); err != nil {
		return result, err
	}
	if record.Pins.DesiredHash != r.DesiredHash {
		result.Code = "policy_changed"
		result.Next = "review YAML changes; runtime migration is required"
		return result, nil
	}
	if observation.Manifest.Mode == "uninstalled" {
		if activityJournal(record) {
			result.State, result.Code = "disabled", "ok"
			result.Next = "gateway service disabled by YAML; state and account retained; set enabled: true and apply to restore verification"
			return result, nil
		}
		result.State = "uninstalled"
		result.Code = "ok"
		result.Next = "service removed; gateway state, credentials and audit history retained"
		return inspectNotificationHostRemoval(r, d, record, result, ledhost.ReadRemovalReceipt)
	}
	if qualification != nil {
		q, e := qualification(ctx, paths.Installation, protocol.Hash(observation.Manifest))
		if e != nil {
			return result, e
		}
		result.Qualification = &q
	}
	if commandDeliveryJournal(record) || admittedCommandHistory(record) && observation.Manifest.Commands != nil && observation.Manifest.Commands.Mode == "drain" {
		result.State, result.Code = "commands_disabled", "ok"
		result.Next = "new command intake disabled by YAML; reporting and retained command results remain; current delivery qualification is required before re-enabling"
		return result, nil
	}
	if observation.Unbound {
		result.State = "waiting_for_producers"
		result.Code = "ok"
		result.Next = "gateway verification service is running; real consumers and delivery qualification are still required"
		return result, nil
	}
	result.State = "needs_qualification"
	result.Code = "qualification_required"
	result.Next = "complete the listed observations, then run deploy notification-worker qualify " + r.Server
	if result.Qualification != nil && result.Qualification.EvidenceComplete {
		result.State, result.Code = "needs_activation", "activation_required"
		result.Next = "run deploy notification-worker " + r.Server + " to admit delivery and connect the declared producer"
		if result.Qualification.AdmissionValid {
			result.State, result.Code = "delivery_admitted", "ok"
			result.Next = "delivery admission is valid; inspect producer ownership and current reporting health"
			if admittedCommandHistory(record) && observation.Manifest.Commands != nil && (observation.Manifest.Commands.Mode == "read" || observation.Manifest.Commands.Mode == "all") {
				result.State, result.Next = "commands_admitted", "declared command admission and receiver are retained; the shared report timer processes commands"
			}
		}
	}
	return result, nil
}

func controlFailure(err error) ControlResult {
	var commands commandActivityPendingError
	if errors.As(err, &commands) {
		return ControlResult{Version: 1, State: "resumable", Code: "command_activity_pending", Next: "command activity is retained: " + commands.Cause.Error() + "; run deploy notification-worker resume " + commands.Server + " with unchanged YAML"}
	}

	if errors.Is(err, backup.ErrSourceReplacementPending) {
		return ControlResult{Version: 1, State: "resumable", Code: "source_replacement_pending", Next: err.Error()}
	}
	if errors.Is(err, backup.ErrSourceRetirementPending) {
		return ControlResult{Version: 1, State: "resumable", Code: "source_retirement_pending", Next: err.Error()}
	}
	if errors.Is(err, errReservedNotificationConsumer) {
		return ControlResult{Version: 1, State: "needs_action", Code: "producer_migration_required", Next: "a backup producer already reserves this gateway policy; restore the retained YAML to finish pending backup setup; changing its gateway policy requires coordinated producer migration"}
	}
	if errors.Is(err, backup.ErrSourceEndpointMismatch) {
		return ControlResult{Version: 1, State: "needs_action", Code: "source_migration_required", Next: backup.ErrSourceEndpointMismatch.Error()}
	}
	var signalResume signalServiceResumeError
	if errors.As(err, &signalResume) {
		return ControlResult{Version: 1, State: "resumable", Code: "signal_service_resume_required", Next: "Signal service resumption is required: " + signalResume.Cause.Error() + "; run deploy notification-worker resume " + signalResume.Server + " with unchanged YAML; the retained account will not be relinked"}
	}

	var backupActivity backupActivityPendingError
	if errors.As(err, &backupActivity) {
		return ControlResult{Version: 1, State: "resumable", Code: "activity_pending", Next: "backup activity is retained; run deploy backup-worker resume " + backupActivity.Server + " with the unchanged YAML"}
	}
	var activity notificationActivityPendingError
	if errors.As(err, &activity) {
		return ControlResult{Version: 1, State: "resumable", Code: "activity_pending", Next: "activity change is unconfirmed: " + activity.Cause.Error() + "; run deploy notification-worker resume " + activity.Server + " with the same declaration"}
	}
	var removal notificationRemovalPendingError
	if errors.As(err, &removal) {
		return ControlResult{Version: 1, State: "resumable", Code: "pending_removal", Next: "removal is unconfirmed: " + removal.Cause.Error() + "; run deploy notification-worker uninstall " + removal.Server + " to resume the retained operation"}
	}
	var pending ledOutputPendingError
	if errors.As(err, &pending) {
		action := pending.Action
		if action == "" {
			action = "resume"
		}
		return ControlResult{Version: 1, State: "cleanup_pending", Code: "led_output_pending", LEDOutputPending: true, Next: "LED cleanup or re-enable is unconfirmed: " + pending.Cause.Error() + "; restore access and run deploy notification-worker " + action + " " + pending.Server + " with the same declaration"}
	}
	return ControlResult{Version: 1, State: "needs_action", Code: "operation_failed", Next: err.Error()}
}
