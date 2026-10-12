// Package version holds the git-hint client version.
package version

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
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

// standardVersion is the SemVer pattern of the HINT data standard (common.schema.json).
var standardVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc|dev)(\.(0|[1-9][0-9]*))?)?$`)

// Supported lists the HINT major.minor versions that the client reads.
var Supported = []string{"0.1"}

// Build describes the running binary.
type Build struct {
	Version       string   `json:"version"`
	Commit        string   `json:"commit"`
	Date          string   `json:"date"`
	Modified      bool     `json:"modified,omitempty"`
	Source        string   `json:"metadata_source"`
	ModuleVersion string   `json:"module_version,omitempty"`
	GoVersion     string   `json:"go_version"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	CGO           bool     `json:"cgo"`
	Standard      string   `json:"standard_version"`
	ReadsMinors   []string `json:"standard_reads"`
}

// Source describes where the metadata of a build comes from.
const (
	SourceRelease   = "release"    // set at link time by the release tool
	SourceGit       = "git"        // go build in a Git checkout: commit and time from Git
	SourceModule    = "go-install" // go install module@version: version from the module, no commit
	SourceUnstamped = "unstamped"  // no metadata (for example a build without .git)
)

// Info returns the build description. Values not set at link time come from the Go build
// information.
func Info() Build {
	bi, _ := debug.ReadBuildInfo()
	return info(bi)
}

func info(bi *debug.BuildInfo) Build {
	b := Build{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(), OS: runtime.GOOS,
		Arch: runtime.GOARCH, Standard: SpecVersion, ReadsMinors: Supported}
	switch {
	case Commit != "":
		b.Source = SourceRelease
	case bi != nil:
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit, b.Source = s.Value, SourceGit
			case "vcs.time":
				b.Date = s.Value
			case "vcs.modified":
				b.Modified = s.Value == "true"
			}
		}
		// go install module@version records the module version, not the commit. The client
		// writes its version into every document, so only a version that the data standard
		// accepts replaces the default; a pseudo-version is reported separately.
		if b.Source == "" && strings.HasPrefix(bi.Main.Version, "v") {
			b.Source = SourceModule
			if v := strings.TrimPrefix(bi.Main.Version, "v"); standardVersion.MatchString(v) {
				b.Version = v
			} else {
				b.ModuleVersion = bi.Main.Version
			}
		}
	}
	if bi != nil {
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				b.CGO = s.Value == "1"
			}
		}
	}
	if b.Source == "" {
		b.Source = SourceUnstamped
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.Date == "" {
		b.Date = "unknown"
	}
	return b
}
