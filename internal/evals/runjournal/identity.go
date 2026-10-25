package runjournal

import (
	"crypto/sha256"
	"embed"
	"fmt"

	githint "github.com/neprel/git-hint"
)

//go:embed types.go configuration.go revision.go parse.go identity.go
var sources embed.FS

// SourceIdentity describes compiled received-history checks, not an admitted origin.
func SourceIdentity() string {
	h := sha256.New()
	for _, name := range []string{"types.go", "configuration.go", "revision.go", "parse.go", "identity.go"} {
		b, _ := sources.ReadFile(name)
		h.Write(b)
	}
	h.Write(githint.SchemaEvalReportV02)
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
