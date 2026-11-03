//go:build !linux

package safefs

import "fmt"

func openAt(_ int, _ string, _ int, _ uint32) (int, error) {
	return -1, fmt.Errorf("local archive access requires the qualified Linux openat2/isolation profile; remote worker administration remains available")
}
func RenameAt(_ int, _ string, _ int, _ string, _ uint) error {
	return fmt.Errorf("local no-replace archive publication requires Linux")
}
