package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Hash logical committed state, including WAL-visible rows. Hashing just the
// main file can miss committed evidence; VACUUM/checkpoint would mutate a review.
func recoveryProjectionHash(ctx context.Context, tx *sql.Tx, limit int64) (string, error) {
	return recoveryProjectionHashWithoutMetadata(ctx, tx, limit, "")
}

// Publication preparation may add exactly one historical anchor. Ignoring that
// row lets its image prove that every original row and row identity is retained.
func recoveryProjectionHashWithoutMetadata(ctx context.Context, tx *sql.Tx, limit int64, excluded string) (string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master ORDER BY type COLLATE BINARY,name COLLATE BINARY")
	if err != nil {
		return "", err
	}
	var schema [][4]string
	var tables []string
	for rows.Next() {
		var row [4]string
		if err = rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			break
		}
		for _, field := range row {
			if !utf8.ValidString(field) {
				err = fmt.Errorf("invalid UTF-8 in recovery schema")
				break
			}
		}
		if err != nil {
			break
		}
		schema = append(schema, row)
		if len(schema) > 1024 {
			err = fmt.Errorf("recovery schema exceeds review budget")
			break
		}
		if row[0] == "table" {
			tables = append(tables, row[1])
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	write := func(value any) error {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		// This is a review budget, not permission to grow the persisted database.
		if int64(len(b)+1) > limit {
			return fmt.Errorf("recovery projection exceeds review budget")
		}
		limit -= int64(len(b) + 1)
		h.Write(b)
		h.Write([]byte{'\n'})
		return nil
	}
	// These header values affect schema interpretation but are not rows in
	// sqlite_master. Retain them in the reviewed projection as well.
	var header struct{ UserVersion, ApplicationID int64 }
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&header.UserVersion); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&header.ApplicationID); err != nil {
		return "", err
	}
	if err = write(header); err != nil {
		return "", err
	}
	if err = write(schema); err != nil {
		return "", err
	}
	total := 0
	for _, name := range tables {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		cols, err := tx.QueryContext(ctx, "SELECT * FROM "+quoted+" LIMIT 0")
		if err != nil {
			return "", err
		}
		names, err := cols.Columns()
		cols.Close()
		if err != nil {
			return "", err
		}
		if len(names) == 0 || len(names) > 256 {
			return "", fmt.Errorf("recovery table exceeds column budget")
		}
		if err = write(struct {
			Table   string
			Columns []string
		}{name, names}); err != nil {
			return "", err
		}
		selection := "*"
		var withoutRowID int
		if err = tx.QueryRowContext(ctx, "SELECT wr FROM pragma_table_list WHERE schema='main' AND name=?", name).Scan(&withoutRowID); err != nil {
			return "", err
		}
		if withoutRowID == 0 {
			alias := ""
			for _, candidate := range []string{"rowid", "_rowid_", "oid"} {
				if !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, candidate) }) {
					alias = candidate
					break
				}
			}
			if alias == "" {
				return "", fmt.Errorf("recovery table shadows every row identity alias")
			}
			selection = alias + ",*"
			names = append([]string{alias}, names...)
		}
		order := make([]string, len(names))
		for i, n := range names {
			q := `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
			order[i] = q + " COLLATE BINARY,typeof(" + q + ")"
		}
		filter := ""
		var args []any
		if name == "metadata" && excluded != "" {
			filter = " WHERE key IS NOT ?"
			args = []any{excluded}
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+selection+" FROM "+quoted+filter+" ORDER BY "+strings.Join(order, ","), args...)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			total++
			if total > 1<<20 {
				err = fmt.Errorf("recovery projection exceeds row budget")
				break
			}
			values := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				break
			}
			typed := make([]struct {
				Type  string
				Value any
			}, len(values))
			for i, v := range values {
				typed[i].Type = fmt.Sprintf("%T", v)
				typed[i].Value = v
				if text, ok := v.(string); ok {
					typed[i].Value = []byte(text)
				}
			}
			if err = write(typed); err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// The same inventory drives review fingerprints and retained checkpoint copies.
func (r *recoveryReconciler) recoveryEvidenceHash(ctx context.Context) (string, error) {
	entries, err := r.recoveryEvidenceInventory(ctx)
	if err != nil {
		return "", err
	}
	return protocol.Hash(entries), nil
}
