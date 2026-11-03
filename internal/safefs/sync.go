package safefs

import (
	"os"
)

func SyncFile(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
