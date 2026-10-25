package library

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"github.com/neprel/git-hint/internal/store"
)

//go:embed resolve_bounded.go
var resolvedReadSource embed.FS

// ResolveReadSourceIdentity is descriptive compiled byte-reader identity only.
func ResolveReadSourceIdentity() string {
	b, _ := resolvedReadSource.ReadFile("resolve_bounded.go")
	h := sha256.New()
	h.Write(b)
	h.Write([]byte(store.BoundedStateSourceIdentity()))
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
