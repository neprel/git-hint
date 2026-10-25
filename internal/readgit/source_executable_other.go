//go:build !unix

package readgit

import "context"

func inspectSourceExecutable(context.Context, string) (sourceExecutableIdentity, int64, error) {
	return sourceExecutableIdentity{}, 0, errSourceInvocation
}
