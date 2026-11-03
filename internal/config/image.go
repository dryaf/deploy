package config

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

func PinnedWorkerImage(image string) error {
	parsed, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("worker image must be an explicit registry image pinned by digest")
	}
	if _, ok := parsed.(reference.Digested); !ok {
		return fmt.Errorf("worker image must be pinned by digest")
	}
	if strings.ContainsAny(image, "\r\n% \t") {
		return fmt.Errorf("invalid image reference")
	}
	return nil
}
