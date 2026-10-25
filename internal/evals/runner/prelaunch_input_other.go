//go:build !darwin && !linux

package runner

import (
	"errors"
	"os"
)

func openRetainedPrelaunchFile(string) (*os.File, error) {
	return nil, errors.New("retained prelaunch admission unsupported")
}
