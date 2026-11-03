package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

var opaqueIDPattern = regexp.MustCompile(`^[asgwbrn]-[0-9a-f]{32}$`)

var DigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func NewID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

func ValidID(id, prefix string) bool {
	return len(id) == 34 && id[:2] == prefix+"-" && opaqueIDPattern.MatchString(id)
}
