package safefs

import (
	"crypto/sha256"
	"fmt"
)

func LockName(service string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(service))) }
