package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

type Options struct {
	TestProvider   string
	Inventory      string
	DryRun         bool
	JSON           bool
	NonInteractive bool
	Catalog        string
}

type Invocation struct {
	Operation, Server string
	Options           Options
}

func Parse(args []string, defaults Options) (Invocation, error) {
	v := Invocation{Operation: "apply", Options: defaults}
	var words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dry-run":
			v.Options.DryRun = true
		case arg == "--json":
			v.Options.JSON = true
		case arg == "--non-interactive":
			v.Options.NonInteractive = true
		case arg == "--provider":
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "-") || v.Options.TestProvider != "" {
				return v, fmt.Errorf("--provider requires one configured provider name")
			}
			v.Options.TestProvider = args[i]
		case strings.HasPrefix(arg, "--provider="):
			if v.Options.TestProvider != "" {
				return v, fmt.Errorf("--provider may be supplied only once")
			}
			v.Options.TestProvider = strings.TrimPrefix(arg, "--provider=")
			if v.Options.TestProvider == "" {
				return v, fmt.Errorf("--provider requires one configured provider name")
			}
		case arg == "--servers":
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return v, fmt.Errorf("--servers requires a path")
			}
			v.Options.Inventory = args[i]
		case strings.HasPrefix(arg, "--servers="):
			v.Options.Inventory = strings.TrimPrefix(arg, "--servers=")
			if v.Options.Inventory == "" {
				return v, fmt.Errorf("--servers requires a path")
			}
		case strings.HasPrefix(arg, "-"):
			return v, fmt.Errorf("unsupported short-command option; use YAML for worker settings")
		default:
			words = append(words, arg)
		}
	}
	if len(words) == 2 {
		v.Operation = words[0]
		v.Server = words[1]
	} else if len(words) == 1 {
		v.Server = words[0]
	} else {
		return v, fmt.Errorf("usage: deploy <worker> [apply|plan|status|configure|import|adopt|resume|uninstall|test|qualify|activate|run] SERVER")
	}
	if !isShortOperation(v.Operation) || !config.ValidInventoryName(v.Server) {
		return v, fmt.Errorf("invalid worker operation or server alias")
	}
	if v.Options.TestProvider != "" && (v.Operation != "test" || !config.ValidInventoryName(v.Options.TestProvider)) {
		return v, fmt.Errorf("--provider selects a configured destination only for notification-worker test")
	}
	if len(words) == 1 && isShortOperation(words[0]) {
		return v, fmt.Errorf("operation requires a server; use apply SERVER for a reserved-name alias")
	}
	if v.Options.Inventory == "" {
		v.Options.Inventory = "server.yaml"
	}
	return v, nil
}
func isShortOperation(s string) bool {
	switch s {
	case "apply", "plan", "status", "configure", "import", "adopt", "resume", "uninstall", "test", "qualify", "activate", "run":
		return true
	}
	return false
}

// Matches preserves explicit legacy/internal operations. Shared command words
// use named-server form only when an alias is supplied and no local/spec flag is
// present. 'apply SERVER' always disambiguates a reserved server alias.
func Matches(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		for _, specialist := range []string{"--local", "--spec", "--report", "--config", "--state-dir", "--archive"} {
			if a == specialist || strings.HasPrefix(a, specialist+"=") {
				return false
			}
		}
	}
	first := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--servers" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			first = args[i]
			break
		}
	}
	if first == "" || strings.HasPrefix(first, "__") {
		return false
	}
	if isShortOperation(first) {
		return true
	}
	return !legacyOperations[first]
}

type CLI struct {
	LinkOutput      io.Writer // Explicit private display injection for controlled callers/tests.
	BackupRemover   BackupRemover
	BackupInstaller BackupInstaller
	SourceTrust     func(context.Context, config.ServerConfig) (string, error)
	Controller      Controller
	Inspector       Inspector
	Prompt          Prompter
	Out, Err        io.Writer
}

type resultWriter struct {
	io.Writer
	wrote bool
}

func (w *resultWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.wrote = w.wrote || n > 0
	return n, err
}

func (c CLI) Run(ctx context.Context, kind string, args []string, defaults Options) error {
	if c.Out == nil {
		c.Out = io.Discard
	}
	tracked := &resultWriter{Writer: c.Out}
	c.Out = tracked
	jsonOutput := defaults.JSON
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
		}
	}
	err := c.run(ctx, kind, args, defaults)
	if err != nil && jsonOutput && !tracked.wrote {
		code := "invalid_input"
		for _, candidate := range []string{"missing_input", "implementation_pending", "provider_setup_required", "signal_setup_required", "consumer_setup_required", "conversation_setup_required"} {
			if strings.HasPrefix(err.Error(), candidate+":") {
				code = candidate
				break
			}
		}
		if ctx.Err() != nil {
			code = "cancelled"
		}
		result := ControlResult{Version: 1, State: "needs_action", Code: code, Next: err.Error()}
		var remote controlResponseError
		if ctx.Err() == nil && errors.As(err, &remote) {
			result = remote.Result
		}
		if writeErr := json.NewEncoder(c.Out).Encode(result); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func (c CLI) run(ctx context.Context, kind string, args []string, defaults Options) error {
	v, err := Parse(args, defaults)
	if err != nil {
		return err
	}
	inv, err := config.ReadInventory(v.Options.Inventory)
	if err != nil {
		return err
	}
	if v.Operation == "import" {
		// An absent block selects default lookup paths for review only. It does
		// not create desired policy or grant apply/adoption authority.
		inv = importLocator(inv, kind, v.Server)
	}
	p, err := DeclarationPlan(inv, kind, v.Server, v.Operation)
	if err != nil {
		return err
	}
	if v.Options.TestProvider != "" {
		n := inv.Servers[v.Server].Desired.Notification
		if kind != "notification-worker" || n == nil {
			return fmt.Errorf("--provider requires a declared notification worker")
		}
		provider, ok := n.Providers[v.Options.TestProvider]
		if !ok || provider.Conversation != "" {
			return fmt.Errorf("--provider must name a configured fixed destination")
		}
		p.Actions = []string{"verify the installed declaration and selected provider", "request one explicit diagnostic for " + v.Options.TestProvider + "; retain unknown outcomes without retries or automatic fallback"}
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.Err == nil {
		c.Err = io.Discard
	}
	if v.Options.DryRun {
		return PrintPlan(c.Out, p, v.Options.JSON)
	}
	if p.Omitted {
		return c.reviewOmittedWorker(ctx, v, inv.Servers[v.Server], p)
	}
	if v.Operation == "qualify" && v.Options.NonInteractive {
		return fmt.Errorf("missing_input: guided qualification requires operator observations; use status to list pending checks")
	}
	if v.Operation == "configure" {
		return c.configure(ctx, kind, v, inv)
	}
	if kind == "notification-worker" && (v.Operation == "apply" || v.Operation == "resume") && config.WorkerEnabled(inv.Servers[v.Server].Desired.Notification.Enabled) && len(inv.Servers[v.Server].Desired.Notification.Providers) > 0 && !sharedActivityStopping(*inv.Servers[v.Server].Desired, v.Server) {
		if err = directNotificationSupport(*inv.Servers[v.Server].Desired); err != nil {
			return err
		}
	}
	if v.Options.NonInteractive && len(p.Missing) > 0 && (v.Operation == "apply" || v.Operation == "resume") {
		_ = PrintPlan(c.Out, p, v.Options.JSON)
		return fmt.Errorf("missing_input: supply the listed YAML inputs before applying")
	}
	if len(p.Missing) > 0 && v.Operation == "apply" && !v.Options.NonInteractive {
		guided := c
		if v.Options.JSON {
			guided.Out = c.Err
		}
		if err = guided.configure(ctx, kind, v, inv); err != nil {
			return err
		}
		inv, err = config.ReadInventory(v.Options.Inventory)
		if err != nil {
			return err
		}
		p, err = DeclarationPlan(inv, kind, v.Server, v.Operation)
		if err != nil {
			return err
		}
		if len(p.Missing) > 0 {
			return fmt.Errorf("missing_input: save the configuration candidate before applying")
		}
	}
	if c.Inspector == nil {
		c.Inspector = SSHInspector{}
	}
	facts, err := c.Inspector.Inspect(ctx, inv.Servers[v.Server])
	if err != nil {
		return err
	}
	catalog := v.Options.Catalog
	if catalog == "" {
		catalog = DefaultCatalog()
	}
	p, err = ResolvePlan(p, inv.Servers[v.Server], facts, catalog)
	if err != nil {
		return err
	}
	if v.Operation == "import" {
		return c.importWorker(ctx, kind, v, inv, facts, p)
	}
	if v.Operation == "qualify" {
		return c.qualifyWorker(ctx, kind, v, inv, facts, p)
	}
	if v.Operation == "activate" {
		return c.activateBackup(ctx, v, inv, facts, p)
	}
	if v.Operation == "run" {
		return c.requestBackupRun(ctx, v, inv, facts, p)
	}
	if v.Operation == "status" {
		inspector, ok := c.Inspector.(StatusInspector)
		if !ok {
			return fmt.Errorf("target status inspection is unavailable")
		}
		observed, statusErr := inspector.Status(ctx, inv.Servers[v.Server], kind, *p.Paths)
		if statusErr != nil {
			return statusErr
		}
		p = ApplyStatus(p, observed)
		if p.Artifacts != nil && p.Code == "admission_unverified" {
			if c.Controller == nil {
				c.Controller = SSHController{}
			}
			r := ControlRequest{Version: 1, Worker: kind, Server: v.Server, Action: "status", Facts: facts, Desired: *inv.Servers[v.Server].Desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash}
			if kind == "backup-worker" && (r.Desired.Backup.Notifications.Worker == "") {
				r.Desired.Notification = nil
			}
			if kind == "backup-worker" {
				var e error
				r.SourceTargets, e = declaredSourceTargets(inv, r.Desired)
				if e != nil {
					return e
				}
			}
			result, e := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
			if e != nil {
				return e
			}
			if v.Options.JSON {
				return json.NewEncoder(c.Out).Encode(result)
			}
			return printControlResult(c.Out, v.Server, kind, result)
		}
		return PrintPlan(c.Out, p, v.Options.JSON)
	}
	if v.Operation == "plan" {
		p, err = c.inspectPlan(ctx, v, inv, facts, p)
		if err != nil {
			return err
		}
		return PrintPlan(c.Out, p, v.Options.JSON)
	}
	if kind == "backup-worker" && v.Operation == "uninstall" {
		return c.removeBackup(ctx, v, inv, p)
	}
	if kind == "notification-worker" && (v.Operation == "uninstall" || v.Operation == "test" || v.Operation == "adopt") {
		if p.Artifacts == nil {
			return fmt.Errorf("helper bundle required for canonical worker operation")
		}
		if c.Controller == nil {
			c.Controller = SSHController{}
		}
		r := ControlRequest{Version: 1, Worker: kind, Server: v.Server, Action: v.Operation, Facts: facts, Desired: *inv.Servers[v.Server].Desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash}
		r.DiagnosticProvider = v.Options.TestProvider
		result, e := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
		if result.Version != 1 {
			if e != nil {
				return e
			}
			return fmt.Errorf("invalid control response")
		}
		if v.Options.JSON {
			if writeErr := json.NewEncoder(c.Out).Encode(result); writeErr != nil {
				return writeErr
			}
			return e
		}
		if err = printControlResult(c.Out, v.Server, kind, result); err != nil {
			return err
		}
		return e
	}
	if kind == "notification-worker" && (v.Operation == "apply" || v.Operation == "resume") {
		if len(p.Missing) > 0 || p.Artifacts == nil {
			_ = PrintPlan(c.Out, p, v.Options.JSON)
			return fmt.Errorf("missing_input: resolve target prerequisites and artifact inputs before applying")
		}
		if config.WorkerEnabled(inv.Servers[v.Server].Desired.Notification.Enabled) && !sharedActivityStopping(*inv.Servers[v.Server].Desired, v.Server) {
			if err = directNotificationSupport(*inv.Servers[v.Server].Desired); err != nil {
				return err
			}
		}
		r := ControlRequest{Version: 1, Worker: kind, Server: v.Server, Action: v.Operation, Facts: facts, Desired: *inv.Servers[v.Server].Desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash, Inputs: map[string][]byte{}}
		if err = prepareOperatorNotificationInputs(&r, *p.Artifacts, facts.Arch); err != nil {
			return err
		}
		if c.Controller == nil {
			c.Controller = SSHController{}
		}
		result, e := c.applyNotificationRequest(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r, v.Options)

		if result.Version != 1 {
			if e == nil {
				e = fmt.Errorf("invalid control response")
			}
			result = ControlResult{Version: 1, State: "resumable", Code: "transport_failed", Next: "inspect and retry the same declaration; the remote outcome is unknown"}
		}
		if v.Options.JSON {
			if writeErr := json.NewEncoder(c.Out).Encode(result); writeErr != nil {
				return writeErr
			}
		} else if result.State != "" {
			if writeErr := printControlResult(c.Out, v.Server, kind, result); writeErr != nil {
				return writeErr
			}
		}
		return e
	}
	if kind == "backup-worker" && (v.Operation == "apply" || v.Operation == "resume" || v.Operation == "adopt") {
		return c.applyBackup(ctx, v, inv, facts, p, catalog)
	}
	// Runtime actions are registered only when the domain coordinator supports
	// them. Never report deployment success after merely validating a declaration.
	p.State = "needs_action"
	p.Code = "implementation_pending"
	p.Next = "worker runtime orchestration is not yet available through the declarative interface"
	if err = PrintPlan(c.Out, p, v.Options.JSON); err != nil {
		return err
	}
	return fmt.Errorf("implementation_pending: declarative %s is not yet available", v.Operation)
}

func printControlResult(out io.Writer, server, kind string, result ControlResult) error {
	var body strings.Builder
	fmt.Fprintf(&body, "%s: %s %s\n", server, kind, result.State)
	if p := result.LEDHostRemoval; p != nil {
		fmt.Fprintf(&body, "  LED host removal review: %s\n", p.ReviewFile)
		for _, action := range p.Plan.Actions {
			fmt.Fprintf(&body, "  Host change: %s\n", action)
		}
		fmt.Fprintf(&body, "  After review, run on %s: %s\n", server, p.Command)
	}
	if p := result.LEDPrerequisite; p != nil {
		fmt.Fprintf(&body, "  Activity LED: %s\n  Host prerequisite review: %s\n", p.Plan.Spec.Device, p.ReviewFile)
		for _, action := range p.Plan.Actions {
			fmt.Fprintf(&body, "  Host change: %s\n", action)
		}
		fmt.Fprintf(&body, "  After review, run on %s: %s\n", server, p.Command)
	}
	if result.LEDOutputPending {
		fmt.Fprintln(&body, "  Status LEDs: cleanup or re-enable pending")
	} else if result.LEDOutput != nil {
		if result.LEDOutput.Disabled {
			fmt.Fprintln(&body, "  Status LEDs: output disabled; incident history retained")
		} else {
			fmt.Fprintln(&body, "  Status LEDs: output enabled; physical qualification is separate")
		}
	}
	if b := result.Backup; b != nil {
		if b.ArchivePath != "" {
			fmt.Fprintf(&body, "  Archive: %s\n", b.ArchivePath)
		}
		if len(b.Sources) != 0 {
			fmt.Fprintf(&body, "  Sources: %s\n", strings.Join(b.Sources, ", "))
		}
		if b.Schedule != "" {
			fmt.Fprintf(&body, "  Schedule: %s, %s\n", b.Schedule, b.Timezone)
		}
		if b.ReportingDisabled {
			fmt.Fprintln(&body, "  Reporting: disabled; incidents remain local")
		}
		if b.SharedReportingConnected {
			fmt.Fprintln(&body, "  Reporting: shared service connected; current delivery health is separate")
		}
		if b.SharedReportingPending {
			fmt.Fprintln(&body, "  Reporting: shared activation pending; legacy sending fenced")
		}
		for _, gate := range b.PendingQualification {
			fmt.Fprintf(&body, "  Qualification needed: %s\n", gate)
		}
		if b.Stage == "complete" && b.Installation != "active" && !b.RecentSuccessfulBackup {
			fmt.Fprintln(&body, "  Activation needs a successful backup from the last hour")
		}
	}
	if q := result.Qualification; q != nil {
		for _, check := range q.Checks {
			if check.Result != "passed" {
				fmt.Fprintf(&body, "  Qualification %s: %s/%s\n", check.Result, check.Scope, check.ID)
			}
		}
		if q.AcknowledgementPending {
			fmt.Fprintln(&body, "  Qualification audit acknowledgement is pending")
		}
	}
	fmt.Fprintf(&body, "  Next: %s\n", result.Next)
	_, err := io.WriteString(out, body.String())
	return err
}

func PrintPlan(out io.Writer, p Plan, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(p)
	}
	var buffer strings.Builder
	fmt.Fprintf(&buffer, "%s: %s %s\n", p.Server, p.Worker, p.State)
	if p.Observed != nil {
		fmt.Fprintf(&buffer, "  Service: %s (%s)\n", p.Observed.ActiveState, p.Observed.LoadState)
	}
	if p.Paths != nil {
		fmt.Fprintf(&buffer, "  Config: %s\n  State: %s\n", p.Paths.Config, p.Paths.State)
		if p.Paths.Archive != "" {
			fmt.Fprintf(&buffer, "  Archive: %s\n", p.Paths.Archive)
		}
	}
	for _, v := range p.Missing {
		fmt.Fprintf(&buffer, "  Required: %s\n", v)
	}
	for _, v := range p.Actions {
		fmt.Fprintf(&buffer, "  Plan: %s\n", v)
	}
	for _, change := range p.Changes {
		fmt.Fprintf(&buffer, "  Change: %s (%s)\n", change.Field, change.Transition)
	}
	for _, v := range p.Unresolved {
		fmt.Fprintf(&buffer, "  Unresolved: %s\n", v)
	}
	if p.Next != "" {
		fmt.Fprintf(&buffer, "  Next: %s\n", p.Next)
	}
	_, err := io.WriteString(out, buffer.String())
	return err
}
func DefaultCatalog() string {
	if executable, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(executable), "workers/catalog.json")
		if _, err = os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if _, err := os.Stat("dist/workers/catalog.json"); err == nil {
		return "dist/workers/catalog.json"
	}
	return ""
}
