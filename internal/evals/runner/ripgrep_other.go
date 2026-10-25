//go:build !darwin && !linux

package runner

import (
	"errors"
	"os"
)

func openRipgrepFile(string) (*os.File, error) {
	return nil, errors.New("offline ripgrep file admission is unsupported")
}
