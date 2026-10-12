// Package version holds the git-hint client version.
package version

// Version is the client version. Release builds set it with
// -ldflags "-X github.com/neprel/git-hint/internal/version.Version=<v>".
var Version = "0.1.0-dev"

// SpecVersion is the HINT data standard version that the client writes.
const SpecVersion = "0.1.0"

// Supported lists the HINT major.minor versions that the client reads.
var Supported = []string{"0.1"}
