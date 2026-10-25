package store

import (
	"crypto/sha256"
	"embed"
	"fmt"
)

//go:embed bounded.go bounded_unix.go bounded_other.go
var boundedSources embed.FS

func BoundedStateSourceIdentity() string {
	h := sha256.New()
	for _, n := range []string{"bounded.go", "bounded_unix.go", "bounded_other.go"} {
		b, _ := boundedSources.ReadFile(n)
		h.Write(b)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
