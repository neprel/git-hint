package qualify

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"github.com/neprel/git-hint/internal/evals/library"
)

//go:embed resolve_inputs.go qualify.go variants.go
var resolvedInputSources embed.FS

// QualifiedInputSourceIdentity describes compiled parsing rules, not qualification attestation.
func QualifiedInputSourceIdentity() string {
	h := sha256.New()
	for _, n := range []string{"resolve_inputs.go", "qualify.go", "variants.go"} {
		b, _ := resolvedInputSources.ReadFile(n)
		h.Write(b)
	}
	h.Write([]byte(library.ResolveReadSourceIdentity()))
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
