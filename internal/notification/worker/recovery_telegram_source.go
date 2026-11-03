package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type telegramRecoveryRecord struct {
	ID          int64
	Hash, State string
	Inbound     *interaction.Inbound
	Mapping     *retainedRequest
}
type telegramRecoveryManifest struct {
	Snapshot SnapshotManifest
	Account  string
	Offset   int64
	Records  uint64
	Digest   string
}

func (v telegramRecoveryRecord) validate(account string, offset int64) error {
	if v.ID < 0 || v.ID >= offset || !safefs.DigestPattern.MatchString(v.Hash) {
		return fmt.Errorf("invalid retained Telegram event")
	}
	if v.State != "pending" && v.State != "converted" && v.State != "rejected" {
		return fmt.Errorf("invalid retained Telegram disposition")
	}
	if v.Inbound == nil {
		if v.State != "rejected" || v.Mapping != nil {
			return fmt.Errorf("missing retained Telegram request")
		}
		return nil
	}
	in := *v.Inbound
	if in.Provider != "telegram" || in.Account != account || in.NativeID != fmt.Sprintf("update-%d", v.ID) ||
		!protocol.ValidToken(in.Actor) || !protocol.ValidToken(in.Conversation) || !safefs.DigestPattern.MatchString(in.DestinationFingerprint) ||
		len(in.Text) > 4096 || !utf8.ValidString(in.Text) || in.SentAt.IsZero() {
		return fmt.Errorf("invalid retained Telegram request")
	}
	if v.Mapping != nil {
		return v.Mapping.validate(in)
	}
	if v.State == "converted" {
		return fmt.Errorf("converted Telegram request lost its mapping")
	}
	return nil
}
func (v telegramRecoveryRecord) disposition() string {
	if v.Inbound == nil || (v.Mapping != nil && v.Mapping.Query != nil && v.Mapping.Query.Kind == "rejected") {
		return "rejected"
	}
	if v.Mapping != nil {
		return "mapped"
	}
	return "unmapped"
}

// Only completed, checksum-verified gateway artifacts are accepted. This never
// asks Telegram to replay/acknowledge updates. Gaps between native update IDs do
// not prove loss or completeness; the summary deliberately makes neither claim.
func readTelegramRecovery(ctx context.Context, path, account string, c Config) (m telegramRecoveryManifest, records []telegramRecoveryRecord, err error) {
	snapshot, dir, err := readSnapshot(ctx, path, c)
	if err != nil {
		return m, nil, err
	}
	defer dir.Close()
	db, err := snapshotDB(dir, "state.db", true)
	if err != nil {
		return m, nil, err
	}
	defer db.Close()
	m = telegramRecoveryManifest{Snapshot: snapshot, Account: account}
	err = db.QueryRowContext(ctx, "SELECT offset FROM telegram_cursors WHERE account=?", account).Scan(&m.Offset)
	if err != nil {
		return m, nil, fmt.Errorf("retained Telegram cursor unavailable: %w", err)
	}
	if m.Offset < 0 {
		return m, nil, fmt.Errorf("invalid retained Telegram cursor")
	}
	rows, err := db.QueryContext(ctx, "SELECT id,hash,body,state FROM provider_inbox WHERE account=? ORDER BY id", account)
	if err != nil {
		return m, nil, err
	}
	bytes := 0
	for rows.Next() {
		var v telegramRecoveryRecord
		var body string
		if err = rows.Scan(&v.ID, &v.Hash, &body, &v.State); err != nil {
			break
		}
		if body != "" {
			v.Inbound = &interaction.Inbound{}
			if err = protocol.Decode([]byte(body), v.Inbound, 8192); err != nil {
				break
			}
		}
		bytes += len(body) + 512
		records = append(records, v)
		if len(records) > 65536 || bytes > 32<<20 {
			err = fmt.Errorf("Telegram recovery history exceeds capture bound")
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return m, nil, err
	}
	// Close the streaming query before reading mappings on the single connection.
	var hashes []string
	bytes = 0
	var highest int64 = -1
	for i := range records {
		v := &records[i]
		if err = ctx.Err(); err != nil {
			return m, nil, err
		}
		if v.Inbound != nil {
			v.Mapping, err = readRetainedRequest(db, *v.Inbound)
			if err != nil {
				return m, nil, err
			}
		}
		if err = v.validate(account, m.Offset); err != nil {
			return m, nil, err
		}
		if v.ID <= highest {
			return m, nil, fmt.Errorf("retained Telegram order changed")
		}
		highest = v.ID
		raw, _ := json.Marshal(v)
		bytes += len(raw)
		if len(raw)+4096 > protocol.MaxRequest || bytes > 32<<20 {
			return m, nil, fmt.Errorf("Telegram recovery evidence exceeds capture bound")
		}
		hashes = append(hashes, protocol.Hash(v))
	}
	if highest+1 != m.Offset {
		return m, nil, fmt.Errorf("retained Telegram cursor lacks its last event")
	}
	m.Records = uint64(len(records))
	m.Digest = interaction.RecoveryEvidenceDigest(hashes)
	// Bind the materialized evidence to the same artifact verified before reading.
	f, err := snapshotFile(dir, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return m, nil, err
	}
	hash, err := safefs.DigestFile(f)
	f.Close()
	if err != nil {
		return m, nil, err
	}
	if hash != snapshot.DatabaseHash {
		return m, nil, fmt.Errorf("Telegram recovery artifact changed during capture")
	}
	return m, records, nil
}

func readTelegramSession(tx *sql.Tx, account string) (s telegramRecoverySession, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM reconciliation_telegram_sessions WHERE account=?", account).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &s, 16384)
	return
}
