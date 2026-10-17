//go:build !unix

package library

import "os"

// Other systems rely on the in-process mutex only (Windows is not a supported platform).
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
