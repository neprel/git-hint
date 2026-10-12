// Package version holds the git-hint client version.
package version

import (
	"runtime"
	"runtime/debug"
)

// Release builds set Version, Commit and Date with
// -ldflags "-X github.com/neprel/git-hint/internal/version.Version=<v> ...".
var (
	// Version is the client version (SemVer, without the leading "v").
	Version = "0.1.0-dev"
	// Commit is the Git commit of the build; empty: taken from the Go build information.
	Commit = ""
	// Date is the commit time of the build (RFC 3339, UTC); empty: taken from the Go build
	// information. It is the commit time, not the build time, so a rebuild gives the same
	// binary.
	Date = ""
)

// SpecVersion is the HINT data standard version that the client writes.
const SpecVersion = "0.1.0"

// Supported lists the HINT major.minor versions that the client reads.
var Supported = []string{"0.1"}

// Build describes the running binary.
type Build struct {
	Version     string   `json:"version"`
	Commit      string   `json:"commit"`
	Date        string   `json:"date"`
	Modified    bool     `json:"modified,omitempty"`
	GoVersion   string   `json:"go_version"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	CGO         bool     `json:"cgo"`
	Standard    string   `json:"standard_version"`
	ReadsMinors []string `json:"standard_reads"`
}

// Info returns the build description. Values not set at link time come from the Go build
// information (a `go build` in a Git checkout records the commit and its time).
func Info() Build {
	b := Build{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), OS: runtime.GOOS,
		Arch: runtime.GOARCH, Standard: SpecVersion, ReadsMinors: Supported}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if b.Commit == "" {
					b.Commit = s.Value
				}
			case "vcs.time":
				if b.Date == "" {
					b.Date = s.Value
				}
			case "vcs.modified":
				b.Modified = b.Modified || s.Value == "true" && Commit == ""
			case "CGO_ENABLED":
				b.CGO = s.Value == "1"
			}
		}
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}
