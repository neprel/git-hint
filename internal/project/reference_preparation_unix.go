//go:build unix

package project

import (
	"golang.org/x/sys/unix"
	"os"
)

// Metadata only; all content still goes through Store.ReadStateBounded. Recheck
// the named inode/link count after the bounded read. This does not claim OS atomicity.
func referenceUnaliasedFile(path string) (os.FileInfo, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() {
		return nil, referenceRefused("artifact_type")
	}
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || st.Nlink != 1 {
		return nil, referenceRefused("artifact_alias")
	}
	return info, nil
}
