//go:build !unix

package store

import (
	"errors"
	"os"
)

// Windows and other systems are not supported in this version; the lock fails closed.
func lockFile(*os.File, bool) error {
	return errors.New("project lock is not supported on this platform")
}

func unlockFile(*os.File) error { return nil }
