package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// ManagedTelegramHandoff consumes one fresh, completed drain for this recovered
// epoch. It preserves the original reconciliation cursor and independently pins
// the newer cutoff. Command admission remains a separate authority.
type ManagedTelegramHandoff struct {
	Version    int                    `json:"version"`
	CommitHash string                 `json:"commit_hash"`
	OwnerHash  string                 `json:"owner_hash"`
	Renewal    TelegramHandoffRenewal `json:"renewal"`
}

// CancelManagedTelegramHandoff closes only an uncommitted handoff attempt. A
// durable SQL decision is instead completed against its exact original intent;
// cancellation never rewrites a cutoff that could already have admitted intake.
func (s *Store) CancelManagedTelegramHandoff(ctx context.Context, recovery ManagedDeliveryRecovery, account, renewalID, renewalHash string) error {
	if !s.verificationOnly || s.installationRevision == "" {
		return fmt.Errorf("handoff cancellation requires managed verification")
	}
	if err := s.RequireManagedDeliveryRecovery(recovery, protocol.Hash(recovery.Owner)); err != nil {
		return err
	}
	name := s.telegramHandoffName(account)
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", name).Scan(&raw)
	if err == nil {
		_, err = s.EnrollManagedTelegramHandoff(ctx, recovery, account, renewalID, renewalHash)
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	session, err := openPublishedTelegramSessionAdmission(ctx, s.dir.File().Name(), s.config, s.recoveryCommitHash, account, s.lease, true)
	if err != nil {
		return err
	}
	defer session.close()
	renewal, err := auditTelegramRenewal(ctx, session, renewalHash)
	if err != nil {
		return err
	}
	if renewal.Binding.ID != renewalID {
		return fmt.Errorf("cancelled Telegram renewal changed")
	}
	want := ManagedTelegramHandoff{1, s.recoveryCommitHash, protocol.Hash(recovery.Owner), renewal}
	var complete, intent ManagedTelegramHandoff
	if e := s.dir.ReadJSON(name+"-complete.json", &complete); e == nil {
		return fmt.Errorf("Telegram handoff SQL anchor lost")
	} else if !os.IsNotExist(e) {
		return e
	}
	err = s.dir.ReadJSON(name+".json", &intent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent != want {
		return fmt.Errorf("cancelled Telegram handoff intent changed")
	}
	archive := name + "-aborted-" + protocol.Hash(want) + ".json"
	var aborted ManagedTelegramHandoff
	if err = s.dir.ReadJSON(archive, &aborted); os.IsNotExist(err) {
		if err = s.dir.WriteJSON(archive, want, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if aborted != want {
		return fmt.Errorf("Telegram handoff cancellation changed")
	}
	if err = unix.Unlinkat(int(s.dir.File().Fd()), name+".json", 0); err != nil {
		return err
	}
	return s.dir.File().Sync()
}

// BindManagedTelegramRenewal audits the current renewal under the already held
// worker lease. Delivery-only recovery may bind it without admitting commands.
func (s *Store) BindManagedTelegramRenewal(ctx context.Context, account, id, hash string) error {
	if s.installationRevision == "" || s.recoveryCommitHash == "" || len(s.runtimeLoops) != 0 {
		return fmt.Errorf("Telegram renewal binding requires offline managed recovery")
	}
	session, err := openPublishedTelegramSessionWithLease(ctx, s.dir.File().Name(), s.config, s.recoveryCommitHash, account, s.lease)
	if err != nil {
		return err
	}
	defer session.close()
	proof, err := auditTelegramRenewal(ctx, session, hash)
	if err != nil {
		return err
	}
	if proof.Binding.ID != id {
		return fmt.Errorf("Telegram renewal binding changed")
	}
	s.telegramRenewalID = id
	return nil
}

func telegramHandoffName(account string) string {
	return "managed-telegram-handoff-" + protocol.Hash(account)
}
func telegramEpochSuffix(proof RecoveryCheckpointProof) string {
	if proof.AncestorHash == "" {
		return ""
	}
	return "-" + protocol.Hash(proof.After)
}
func (s *Store) telegramHandoffName(account string) string {
	return telegramHandoffName(account) + telegramEpochSuffix(s.recoveryProof)
}
func (s *publishedTelegramSession) renewalIndex() string {
	return epochRenewalIndex(s.cursor.Drain.Account, s.proof)
}
func epochRenewalIndex(account string, proof RecoveryCheckpointProof) string {
	return "telegram-renewal-" + protocol.Hash(account) + telegramEpochSuffix(proof) + ".json"
}

func (h ManagedTelegramHandoff) validate() error {
	r := h.Renewal
	hash := r.RenewalHash
	r.RenewalHash = ""
	if h.Version != 1 || !safefs.DigestPattern.MatchString(h.CommitHash) || !safefs.DigestPattern.MatchString(h.OwnerHash) || hash != protocol.Hash(r) || r.Binding.CommitHash != h.CommitHash || r.Cursor.Validate() != nil || r.IntakeEnabled || !safefs.ValidID(r.Binding.ID, "n") || r.Binding.Account != r.Cursor.Drain.Account {
		return fmt.Errorf("invalid managed Telegram handoff")
	}
	return nil
}

func readManagedTelegramHandoff(ctx context.Context, d *safefs.Directory, db *sql.DB, account string) (*ManagedTelegramHandoff, error) {
	var anchor recoveryPublicationAnchor
	var anchorRaw string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", publicationAnchorKey).Scan(&anchorRaw); err != nil {
		return nil, err
	}
	if err := protocol.Decode([]byte(anchorRaw), &anchor, 65536); err != nil {
		return nil, err
	}
	if anchor.Version != 1 || anchor.Mode != "fenced" || !anchor.Proof.After.Valid() {
		return nil, fmt.Errorf("Telegram epoch anchor changed")
	}
	name := telegramHandoffName(account) + telegramEpochSuffix(anchor.Proof)
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", name).Scan(&raw)
	var intent, complete ManagedTelegramHandoff
	ie := d.ReadJSON(name+".json", &intent)
	ce := d.ReadJSON(name+"-complete.json", &complete)
	if err == sql.ErrNoRows && os.IsNotExist(ie) && os.IsNotExist(ce) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ie != nil {
		return nil, ie
	}
	if ce != nil {
		return nil, ce
	}
	var h ManagedTelegramHandoff
	if protocol.Decode([]byte(raw), &h, 32768) != nil || h.validate() != nil || h != intent || h != complete || h.Renewal.Binding.Account != account {
		return nil, fmt.Errorf("managed Telegram handoff anchors changed")
	}
	var aborted ManagedTelegramHandoff
	if e := d.ReadJSON(name+"-aborted-"+protocol.Hash(h)+".json", &aborted); e == nil {
		return nil, fmt.Errorf("cancelled Telegram handoff cannot grant intake")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	var ownerRaw string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", managedDeliveryOwnerKey).Scan(&ownerRaw); err != nil {
		return nil, err
	}
	var owner ManagedDeliveryOwner
	if protocol.Decode([]byte(ownerRaw), &owner, 8192) != nil || owner.validate() != nil || protocol.Hash(owner) != h.OwnerHash {
		return nil, fmt.Errorf("Telegram handoff owner changed")
	}
	var head telegramRenewalState
	if _, err = readRenewalJSON(d, epochRenewalIndex(account, anchor.Proof), &head, 16384); err != nil {
		return nil, err
	}
	if head.Binding != h.Renewal.Binding {
		return nil, fmt.Errorf("Telegram handoff renewal superseded")
	}
	return &h, nil
}

func (s *Store) EnrollManagedTelegramHandoff(ctx context.Context, recovery ManagedDeliveryRecovery, account, renewalID, renewalHash string) (ManagedTelegramHandoff, error) {
	return s.enrollManagedTelegramHandoff(ctx, recovery, account, renewalID, renewalHash, telegramRenewalClock{time.Now, process.HostClock}, nil)
}

func (s *Store) enrollManagedTelegramHandoff(ctx context.Context, recovery ManagedDeliveryRecovery, account, renewalID, renewalHash string, clock telegramRenewalClock, fault func(string) error) (out ManagedTelegramHandoff, err error) {
	if !s.verificationOnly || s.installationRevision == "" || s.config.Interaction == nil {
		return out, fmt.Errorf("Telegram handoff requires stopped managed recovery verification")
	}
	if err = s.RequireManagedDeliveryRecovery(recovery, protocol.Hash(recovery.Owner)); err != nil {
		return out, err
	}
	// The exact unfinished activation alone calls this recovery path. Published
	// runtime and completed lifecycle retries require every anchor instead.
	session, err := openPublishedTelegramSessionAdmission(ctx, s.dir.File().Name(), s.config, s.recoveryCommitHash, account, s.lease, true)
	if err != nil {
		return out, err
	}
	defer session.close()
	renewal, err := auditTelegramRenewal(ctx, session, renewalHash)
	if err != nil {
		return out, err
	}
	if renewal.Binding.ID != renewalID {
		return out, fmt.Errorf("Telegram handoff renewal changed")
	}
	out = ManagedTelegramHandoff{1, s.recoveryCommitHash, protocol.Hash(recovery.Owner), renewal}
	name := s.telegramHandoffName(account)
	var aborted ManagedTelegramHandoff
	if e := s.dir.ReadJSON(name+"-aborted-"+protocol.Hash(out)+".json", &aborted); e == nil {
		return out, fmt.Errorf("Telegram handoff was cancelled; renew before enabling commands")
	} else if !os.IsNotExist(e) {
		return out, e
	}
	intentExists, completeExists := false, false
	for _, suffix := range []string{".json", "-complete.json"} {
		var old ManagedTelegramHandoff
		e := s.dir.ReadJSON(name+suffix, &old)
		if suffix == ".json" && e == nil {
			intentExists = true
		}
		if suffix == "-complete.json" && e == nil {
			completeExists = true
		}
		if e == nil && old != out {
			return out, fmt.Errorf("Telegram handoff cannot replace an existing cutoff")
		}
		if e != nil && !os.IsNotExist(e) {
			return out, e
		}
	}
	var raw string
	e := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", name).Scan(&raw)
	committed := e == nil
	if e == nil {
		var old ManagedTelegramHandoff
		if protocol.Decode([]byte(raw), &old, 32768) != nil || old != out {
			return out, fmt.Errorf("Telegram handoff SQL changed")
		}
		var intent ManagedTelegramHandoff
		if e = s.dir.ReadJSON(name+".json", &intent); e != nil || intent != out {
			return out, fmt.Errorf("Telegram handoff lost intent")
		}
	} else if e != sql.ErrNoRows {
		return out, e
	}
	if e == sql.ErrNoRows {
		var complete ManagedTelegramHandoff
		if ce := s.dir.ReadJSON(name+"-complete.json", &complete); ce == nil {
			return out, fmt.Errorf("Telegram handoff lost SQL anchor")
		} else if !os.IsNotExist(ce) {
			return out, ce
		}
	}
	if committed && !completeExists {
		head, e := s.commandSQLHead()
		if e != nil {
			return out, e
		}
		if head != nil && head.Identity == s.identity && head.ActivationHash != s.recoveryCommitHash {
			return out, fmt.Errorf("admitted commands lost Telegram handoff completion")
		}
	}
	if !committed {
		if _, err = session.verifyRenewedHandoff(ctx, renewalHash, clock); err != nil {
			return out, err
		}
	}
	if !intentExists {
		if err = s.dir.WriteJSON(name+".json", out, false); err != nil {
			return out, err
		}
	}
	if err = recoveryBoundary(fault, "telegram_handoff_intent"); err != nil {
		return out, err
	}
	if !committed {
		if _, err = session.verifyRenewedHandoff(ctx, renewalHash, clock); err != nil {
			return out, err
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = s.db.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING", name, string(data)); err != nil {
		return out, err
	}
	if err = recoveryBoundary(fault, "telegram_handoff_sql"); err != nil {
		return out, err
	}
	var complete ManagedTelegramHandoff
	if e := s.dir.ReadJSON(name+"-complete.json", &complete); os.IsNotExist(e) {
		if err = s.dir.WriteJSON(name+"-complete.json", out, false); err != nil {
			return out, err
		}
	} else if e != nil {
		return out, e
	}
	if err = recoveryBoundary(fault, "telegram_handoff_complete"); err != nil {
		return out, err
	}
	return out, s.RequireManagedTelegramHandoff(ctx, out, protocol.Hash(out))
}

func (s *Store) RequireManagedTelegramHandoff(ctx context.Context, want ManagedTelegramHandoff, hash string) error {
	if want.validate() != nil || protocol.Hash(want) != hash || want.CommitHash != s.recoveryCommitHash || want.Renewal.Cursor.Drain.Identity != s.identity {
		return fmt.Errorf("Telegram runtime handoff pins changed")
	}
	if err := s.checkRetirementLease(); err != nil {
		return err
	}
	got, err := readManagedTelegramHandoff(ctx, s.dir, s.db, want.Renewal.Binding.Account)
	if err != nil {
		return err
	}
	if got == nil || *got != want {
		return fmt.Errorf("Telegram runtime handoff missing")
	}
	return nil
}

// Commands always use the newer consumed cutoff, including conversion of an
// inbox row restored before the renewal. Original reconciliation proof stays
// byte-for-byte intact for independent publication audits.
func telegramAdmissionCursor(tx *sql.Tx, account string) (*TelegramRecoveryCursor, error) {
	base, err := readTelegramRecoveryCursor(tx, account)
	if err != nil || base == nil {
		return base, err
	}
	var raw string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key=?", telegramHandoffName(account)).Scan(&raw)
	if err == sql.ErrNoRows {
		return base, nil
	}
	if err != nil {
		return nil, err
	}
	var h ManagedTelegramHandoff
	if protocol.Decode([]byte(raw), &h, 32768) != nil || h.validate() != nil || h.Renewal.Binding.Account != account || h.Renewal.Cursor.Drain.Identity != base.Drain.Identity || h.Renewal.Cursor.RequestsHash != base.RequestsHash || h.Renewal.Cursor.NotBefore.Before(base.NotBefore) {
		return nil, fmt.Errorf("Telegram admission cutoff changed")
	}
	return &h.Renewal.Cursor, nil
}
