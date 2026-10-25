//go:build !unix

package store

import (
	"context"
	"os"
)

// These new exact-source APIs refuse when no-follow/nonblocking ancestry is unavailable.
func openBoundedState(string, []string, bool) (*boundedStateSource, error) {
	return nil, ErrBoundedState
}
func writeBoundedState(context.Context, string, []string, []byte) error { return ErrBoundedState }
func openBoundedLock(*Store) (*os.File, error)                          { return nil, ErrBoundedState }

func boundedRootIdentity(string) (string, error) { return "", ErrBoundedState }

func ensureBoundedStateDir(context.Context, string, []string) error { return ErrBoundedState }
