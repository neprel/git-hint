//go:build !unix

package runner

import "context"

func referenceSocketPinAt(context.Context, string) (referenceSocketPin, error) {
	return referenceSocketPin{}, referencePolicyFailure("platform_unsupported")
}
