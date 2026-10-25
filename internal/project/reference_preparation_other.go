//go:build !unix

package project

import "os"

func referenceUnaliasedFile(string) (os.FileInfo, error) {
	return nil, referenceRefused("artifact_platform_unsupported")
}
