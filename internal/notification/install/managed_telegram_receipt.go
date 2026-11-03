package install

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type managedTelegramReceipt struct {
	Operation    string                        `json:"operation"`
	ManifestHash string                        `json:"manifest_hash"`
	Renewal      worker.TelegramHandoffRenewal `json:"renewal"`
}

func prepareManagedTelegramRequest(ctx context.Context, d *safefs.Directory, j managedJournal, c Runtime) (out managedTelegramRequest, err error) {
	m := j.Intent.After
	name := "operations/" + j.Intent.ID + "/telegram-request.json"
	err = d.ReadJSON(name, &out)
	if os.IsNotExist(err) {
		head, e := worker.ReviewTelegramRenewalHead(ctx, m.Paths.State, c.Worker, m.RecoveryCommitHash, c.Telegram.Account)
		if e != nil {
			return out, e
		}
		if !head.Complete || head.ID != j.Intent.Before.TelegramRenewalID {
			return out, fmt.Errorf("managed Telegram request missing or worker predecessor changed")
		}
		out = managedTelegramRequest{j.Intent.ID, protocol.Hash(m), worker.TelegramRenewalRequest{ID: j.Intent.ID, PreviousHash: head.Hash}}
		err = d.WriteJSON(name, out, false)
	}
	if err != nil {
		return out, err
	}
	if out.Operation != j.Intent.ID || out.ManifestHash != protocol.Hash(m) || out.Request.ID != j.Intent.ID || !safefs.DigestPattern.MatchString(out.Request.PreviousHash) {
		return out, fmt.Errorf("managed Telegram request changed")
	}
	return out, nil
}

// Validate the installation copy without acquiring the worker lease. Completed
// resume/status also work while the verification service holds that lease.
func readManagedTelegramReceipt(d *safefs.Directory, m ManagedManifest) (out managedTelegramReceipt, err error) {
	if m.TelegramRenewalID == "" {
		return out, fmt.Errorf("no managed Telegram renewal")
	}
	var intent managedIntent
	base := "operations/" + m.TelegramRenewalID + "/"
	if err = d.ReadJSON(base+"intent.json", &intent); err != nil {
		return
	}
	if err = (managedJournal{Intent: intent, Phase: "complete"}).validate(); err != nil {
		return
	}
	a := intent.After
	if intent.Operation != "renew-telegram" || intent.ID != m.TelegramRenewalID || a.Installation != m.Installation || a.Root != m.Root || a.Paths.State != m.Paths.State || a.UnitDirectory != m.UnitDirectory || a.Identity != m.Identity || (a.ConfigHash != m.ConfigHash && !managedSignalConfigBridge(d, a, m)) || a.Schema != m.Schema || a.RecoveryCommitHash != m.RecoveryCommitHash {
		return out, fmt.Errorf("managed Telegram receipt authority changed")
	}
	var request managedTelegramRequest
	if err = d.ReadJSON(base+"telegram-request.json", &request); err != nil {
		return
	}
	if request.Operation != intent.ID || request.ManifestHash != protocol.Hash(a) || request.Request.ID != intent.ID || !safefs.DigestPattern.MatchString(request.Request.PreviousHash) {
		return out, fmt.Errorf("managed Telegram request evidence changed")
	}
	if err = d.ReadJSON(base+"telegram-receipt.json", &out); err != nil {
		return
	}
	p := out.Renewal
	hash := p.RenewalHash
	p.RenewalHash = ""
	if out.Operation != intent.ID || out.ManifestHash != protocol.Hash(a) || !safefs.DigestPattern.MatchString(hash) || protocol.Hash(p) != hash || p.IntakeEnabled || p.Binding.ID != intent.ID || p.Binding.PreviousID != intent.Before.TelegramRenewalID || p.Binding.PreviousHash != request.Request.PreviousHash || p.Binding.CommitHash != m.RecoveryCommitHash || p.Cursor.Drain.Identity != m.Identity || p.Binding.Account != p.Cursor.Drain.Account {
		return out, fmt.Errorf("managed Telegram completion changed")
	}
	err = p.Cursor.Validate()
	return
}

func verifyManagedTelegramAudit(ctx context.Context, d *safefs.Directory, m ManagedManifest, c Runtime) error {
	if m.TelegramRenewalID == "" {
		return nil
	}
	receipt, err := readManagedTelegramReceipt(d, m)
	if err != nil {
		return err
	}
	if c.Telegram == nil || receipt.Renewal.Binding.Account != c.Telegram.Account {
		return fmt.Errorf("managed Telegram account changed")
	}
	actual, err := worker.AuditTelegramHandoffRenewal(ctx, m.Paths.State, c.Worker, m.RecoveryCommitHash, c.Telegram.Account, receipt.Renewal.RenewalHash)
	if err != nil {
		return err
	}
	if actual != receipt.Renewal {
		return fmt.Errorf("managed and worker Telegram completions differ")
	}
	return nil
}
