package interaction

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Validate is shared by both ledgers. A worker must validate its result before
// committing domain admission, so a malformed result cannot strand that effect.
func (r Result) Validate() error {
	hash, err := hex.DecodeString(r.CommandHash)
	if !protocol.ValidToken(r.CommandID) || err != nil || len(hash) != 32 || r.CommandHash != strings.ToLower(r.CommandHash) || !protocol.Sequence(r.Revision) || r.Scope.Validate() != nil || !utf8.ValidString(r.Text) || len(r.Text) > 3000 || (r.JobID != "" && !protocol.ValidToken(r.JobID)) {
		return fmt.Errorf("invalid command result")
	}
	switch r.Phase {
	case "prepare":
		if r.JobID != "" || !slices.Contains([]string{"needs_confirmation", "rejected"}, r.State) {
			return fmt.Errorf("invalid preparation result")
		}
		if r.State == "needs_confirmation" {
			if r.Preparation == nil {
				return fmt.Errorf("missing preparation")
			}
			h, err := hex.DecodeString(r.Preparation.Hash)
			if err != nil || len(h) != 32 || r.Preparation.Hash != strings.ToLower(r.Preparation.Hash) || len(r.Preparation.Summary) > 2048 || !utf8.ValidString(r.Preparation.Summary) || r.Preparation.ExpiresAt.IsZero() {
				return fmt.Errorf("invalid preparation")
			}
		} else if r.Preparation != nil {
			return fmt.Errorf("unexpected preparation")
		}
	case "execute":
		if r.Preparation != nil || !slices.Contains([]string{"running", "succeeded", "failed", "rejected", "outcome_unknown"}, r.State) {
			return fmt.Errorf("invalid execution result")
		}
		if r.State == "running" && r.JobID == "" {
			return fmt.Errorf("running result requires durable job")
		}
	default:
		return fmt.Errorf("invalid result phase")
	}
	return nil
}
