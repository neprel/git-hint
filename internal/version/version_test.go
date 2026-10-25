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

func TestConditionalWriterMetadata(t *testing.T) {
	b := info(nil)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["standard_draft_writes"]) != `"0.3.0"` || string(wire["standard_writes"]) != `["0.1.0","0.2.0","0.3.0"]` || string(wire["standard_reads"]) != `["0.1","0.2","0.3"]` {
		t.Fatalf("metadata %s", raw)
	}
	const conditions = `[{"version":"0.2.0","fields":["observation context measurement","declared task family","source configuration","model call (MODEL_CALL, request_kind, correlation.call_ref)","collector closure (collection)","usage normalization","declared plan application","run correlation (correlation.run_ref)"]},{"version":"0.3.0","fields":["keyed model ledger (model_ledger)","keyed project membership (membership)"]}]`
	if string(wire["standard_draft_conditions"]) != conditions {
		t.Fatalf("conditions %s", wire["standard_draft_conditions"])
	}
	if DraftVersion != "0.2.0" || SpecVersion != "0.1.0" || Version != "0.1.0-dev" {
		t.Fatal("writer or client constants changed")
	}
	b.DraftConditions[0].Fields[0] = "changed"
	b.ReadsMinors[0] = "changed"
	if next := info(nil); next.DraftConditions[0].Fields[0] != "observation context measurement" || next.ReadsMinors[0] != "0.1" {
		t.Fatal("metadata aliases shared state")
	}
}
