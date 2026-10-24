//go:build !unix

package library

import "os"

// Strict worktree capture is Unix-only. The legacy portable path still checks
// regular type and exact descriptor/path identity before any content read.
func openCachedRegular(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
