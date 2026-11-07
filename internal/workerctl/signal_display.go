package workerctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dryaf/deploy/internal/config"
)

type interactiveController interface {
	ApplyInteractive(context.Context, config.ServerConfig, Artifact, ControlRequest, io.Writer) (ControlResult, error)
}

func (c CLI) applyNotificationRequest(ctx context.Context, server config.ServerConfig, helper Artifact, r ControlRequest, options Options) (ControlResult, error) {
	result, err := c.applyNotificationWithDisplay(ctx, server, helper, r, options)
	if result.Code != "signal_link_review_required" || result.SignalReview == "" || options.NonInteractive || options.JSON {
		return result, err
	}
	prompt := c.Prompt
	if prompt == nil {
		prompt = TerminalPrompt{Input: os.Stdin, Output: c.Err}
	}
	answer, askErr := prompt.Ask(ctx, "The previous Signal link has an uncertain outcome. Inspect linked devices on your phone. Authorize native inspection of that retained account without starting another link? [yes/no]", false)
	if askErr != nil {
		return result, askErr
	}
	if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
		return result, fmt.Errorf("Signal link review deferred; retained account remains fenced")
	}
	r.SignalReview = result.SignalReview
	// One explicit decision, one retry. A lost review response is reconciled from
	// its retained domain record; never loop through prompts or launch linking.
	return c.applyNotificationWithDisplay(ctx, server, helper, r, options)
}

func (c CLI) applyNotificationWithDisplay(ctx context.Context, server config.ServerConfig, helper Artifact, r ControlRequest, options Options) (ControlResult, error) {
	if _, ok := r.Desired.Notification.Providers["signal"]; ok && config.WorkerEnabled(r.Desired.Notification.Enabled) && !options.NonInteractive && !options.JSON {
		if controller, ok := c.Controller.(interactiveController); ok {
			display := c.LinkOutput
			if display == nil {
				// Never send linking payloads to ordinary stdout/stderr logs. The controlling
				// terminal is explicit; absence leaves the reserved account in needs_action.
				tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
				if err == nil {
					defer tty.Close()
					display = tty
				}
			}
			if display != nil {
				if _, err := fmt.Fprintln(display, "If linking is required, use the private Signal link below on your device."); err != nil {
					return ControlResult{}, fmt.Errorf("private linking display unavailable")
				}
				return controller.ApplyInteractive(ctx, server, helper, r, display)
			}
		}
	}
	return c.Controller.Apply(ctx, server, helper, r)
}
