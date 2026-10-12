package version

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestInfoMetadataSources(t *testing.T) {
	setting := func(kv ...string) []debug.BuildSetting {
		var s []debug.BuildSetting
		for i := 0; i < len(kv); i += 2 {
			s = append(s, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return s
	}
	for _, c := range []struct {
		name                       string
		bi                         *debug.BuildInfo
		version, commit, date, src string
		modified                   bool
	}{
		{"git checkout", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"},
			Settings: setting("vcs.revision", "abc123", "vcs.time", "2026-10-12T17:00:00Z", "vcs.modified", "true")},
			Version, "abc123", "2026-10-12T17:00:00Z", SourceGit, true},
		{"go install module@version", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, "0.1.0", "unknown", "unknown", SourceModule, false},
		{"pseudo-version", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.1-0.20261012170000-abcdef123456"}},
			Version, "unknown", "unknown", SourceModule, false},
		{"no .git", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, Version, "unknown", "unknown", SourceUnstamped, false},
		{"no build info", nil, Version, "unknown", "unknown", SourceUnstamped, false},
	} {
		b := info(c.bi)
		if b.Version != c.version || b.Commit != c.commit || b.Date != c.date || b.Source != c.src || b.Modified != c.modified {
			t.Errorf("%s: %+v", c.name, b)
		}
	}
	if b := info(&debug.BuildInfo{Main: debug.Module{Version: "v0.1.1-0.20261012170000-abcdef123456"}}); b.ModuleVersion != "v0.1.1-0.20261012170000-abcdef123456" {
		t.Errorf("pseudo-version not reported: %+v", b)
	}
	if !standardVersion.MatchString(Version) {
		t.Errorf("default version %q is not a standard version", Version)
	}
	Commit, Date, Version = "def456", "2026-10-12T18:00:00Z", "0.1.0"
	defer func() { Commit, Date, Version = "", "", "0.1.0-dev" }()
	b := info(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: setting("vcs.revision", "other")})
	if b.Commit != "def456" || b.Date != "2026-10-12T18:00:00Z" || b.Version != "0.1.0" || b.Source != SourceRelease {
		t.Errorf("release: %+v", b)
	}
}

func TestStandardVersionMatchesSchema(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0.1", "common.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Defs["SemVer"].Pattern != standardVersion.String() {
		t.Fatalf("pattern %q differs from the schema %q", standardVersion, s.Defs["SemVer"].Pattern)
	}
}
