package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalDelivery retains the exact private references approved for an existing
// native service. Its hash names an immutable service receipt, not a new account.
type SignalDelivery struct {
	ServiceHash   string `json:"service_hash"`
	AccountFile   string `json:"account_file"`
	AccountHash   string `json:"account_hash"`
	RecipientFile string `json:"recipient_file"`
	RecipientHash string `json:"recipient_hash"`
}

func (s SignalDelivery) Hash() string { return protocol.Hash(s) }
func (s SignalDelivery) Values() (string, string, error) {
	if !safefs.DigestPattern.MatchString(s.ServiceHash) || !safefs.DigestPattern.MatchString(s.AccountHash) || !safefs.DigestPattern.MatchString(s.RecipientHash) || !filepath.IsAbs(s.AccountFile) || !filepath.IsAbs(s.RecipientFile) {
		return "", "", fmt.Errorf("invalid Signal delivery references")
	}

	values := []string{}
	for _, v := range []struct{ p, h string }{{s.AccountFile, s.AccountHash}, {s.RecipientFile, s.RecipientHash}} {
		raw, err := readSecretBytes(v.p)
		if err != nil {
			return "", "", err
		}
		hash := sha256.Sum256(raw)
		if hex.EncodeToString(hash[:]) != v.h {
			return "", "", fmt.Errorf("retained Signal delivery input changed")
		}
		values = append(values, strings.TrimSpace(string(raw)))
	}
	return values[0], values[1], nil
}
