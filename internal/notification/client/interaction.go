package client

import (
	"context"
	"encoding/json"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (c Client) CommandCall(ctx context.Context, p protocol.Producer, operation string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.Call(ctx, protocol.Request{Operation: operation, Producer: p, Interaction: raw}, out)
	return err
}
func (c Client) EnqueueTransitions(ctx context.Context, b protocol.TransitionBatch) (r protocol.TransitionReceipt, err error) {
	_, err = c.Call(ctx, protocol.Request{Operation: "fault-transition.enqueue", Producer: b.Producer, Transitions: &b}, &r)
	return
}
func (c Client) Commands(ctx context.Context, p protocol.Producer) (v []interaction.View, err error) {
	err = c.CommandCall(ctx, p, "command.list", struct{}{}, &v)
	return
}
func (c Client) SubmitCommandResult(ctx context.Context, p protocol.Producer, r interaction.Result) (receipt interaction.ResultReceipt, err error) {
	err = c.CommandCall(ctx, p, "command.result", r, &receipt)
	return
}
