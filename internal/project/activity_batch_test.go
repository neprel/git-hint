package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func batchFixture(t *testing.T, run string) (*store.Store, activity.CLIRootImport) {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	root := t.TempDir()
	testkit.Git(t, root, "init", "-q")
	s, _, err := store.Init(root, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s, decodedBatch(t, s, run)
}
func decodedBatch(t *testing.T, s *store.Store, run string) activity.CLIRootImport {
	t.Helper()
	var cfg activity.Configuration
	if err := json.Unmarshal([]byte(`{"agent":{"name":"opencode","version":{"state":"known","value":"2.0.26"}},"model":{"state":"unknown"},"harness":{"kind":"hint-docker","version":{"state":"known","value":"0.1.0"}},"context_policy":{"state":"disabled"},"config_refs":{"state":"known","refs":[]},"budget":{"max_requests":{"state":"known","value":5},"max_seconds":{"state":"known","value":30}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "activity", "testdata", "cli-root", "p3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := activity.DecodeCLIRoot(strings.NewReader(strings.ReplaceAll(string(raw), "synthetic-session-1", "synthetic-session-"+run)), activity.CLIRootOptions{Key: s.Key(), AgentVersion: "2.0.26", Configuration: &cfg,
		TaskID: TaskID(s.Key(), "task"), TaskFamilyID: activity.FamilyRef(s.Key(), "family"), RunRef: activity.RunRef(s.Key(), run), Cwd: "/app", CaptureComplete: true,
		Resolve: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
func TestActivityBatchAtomicReplayAndLegacySpool(t *testing.T) {
	s, b := batchFixture(t, "run")
	legacy := decodedBatch(t, s, "legacy")
	if err := RegisterSource(s, legacy.Source); err != nil {
		t.Fatal(err)
	}
	for _, o := range legacy.Observations {
		if err := AppendObservation(s, o); err != nil {
			t.Fatal(err)
		}
	}
	// A historical spool's final JSON line need not have a newline.
	spool := s.Path("state", "activity", spoolFile)
	raw, _ := os.ReadFile(spool)
	os.WriteFile(spool, bytesTrimNewline(raw), 0o600)
	for i := 0; i < 2; i++ {
		if err := WriteActivityBatch(s, b.Source, b.Observations); err != nil {
			t.Fatal(err)
		}
	}
	obs, damage, err := ReadSpoolChecked(s)
	if err != nil || damage.Any() || len(obs) != len(b.Observations)+len(legacy.Observations) {
		t.Fatalf("read: %d %+v %v", len(obs), damage, err)
	}
	sources, err := Sources(s)
	if err != nil || len(sources) != 2 {
		t.Fatalf("sources %v %v", sources, err)
	}
	info, err := os.Stat(s.Path("state", "activity", activityBatchDir, b.Source.SourceID+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}
func bytesTrimNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		return b[:len(b)-1]
	}
	return b
}
func TestActivityBatchRejectsInvalidDuplicateAndConflictWithoutPartialWrite(t *testing.T) {
	s, b := batchFixture(t, "run")
	duplicate := append(append([]activity.Observation{}, b.Observations...), b.Observations[0])
	if WriteActivityBatch(s, b.Source, duplicate) == nil {
		t.Fatal("duplicate accepted")
	}
	source := b.Source
	source.Agent = "HINTCANARY"
	if WriteActivityBatch(s, source, b.Observations) == nil {
		t.Fatal("invalid source accepted")
	}
	obs, _, err := ReadSpoolChecked(s)
	sources, se := Sources(s)
	if err != nil || se != nil || len(obs) != 0 || len(sources) != 0 {
		t.Fatal("partial commit")
	}
	if err := WriteActivityBatch(s, b.Source, b.Observations); err != nil {
		t.Fatal(err)
	}
	name := s.Path("state", "activity", activityBatchDir, b.Source.SourceID+".json")
	before, _ := os.ReadFile(name)
	changed := append([]activity.Observation{}, b.Observations...)
	changed[0].Outcome.Status = "failed"
	if WriteActivityBatch(s, b.Source, changed) == nil {
		t.Fatal("conflicting replay accepted")
	}
	after, _ := os.ReadFile(name)
	if string(before) != string(after) {
		t.Fatal("conflict changed existing batch")
	}
	changedSource := b.Source
	changedSource.AgentVersion = nil
	if RegisterSource(s, changedSource) == nil {
		t.Fatal("legacy registration rewrote an immutable batch source")
	}

	if strings.Contains(string(after), "HINTCANARY") || strings.Contains(string(after), "/app/") {
		t.Fatal("raw data in minimized batch")
	}
}
func TestActivityBatchCorruptionRefusesBothReaders(t *testing.T) {
	s, b := batchFixture(t, "run")
	if err := WriteActivityBatch(s, b.Source, b.Observations); err != nil {
		t.Fatal(err)
	}
	file := s.Path("state", "activity", activityBatchDir, b.Source.SourceID+".json")
	var batch activityBatch
	s.ReadJSON(&batch, "state", "activity", activityBatchDir, b.Source.SourceID+".json")
	batch.Digest = "sha256:" + strings.Repeat("0", 64)
	raw, _ := json.Marshal(batch)
	os.WriteFile(file, raw, 0o600)
	if _, _, err := ReadSpoolChecked(s); err == nil {
		t.Fatal("corrupt observations accepted")
	}
	if _, err := Sources(s); err == nil {
		t.Fatal("corrupt source accepted")
	}
}
func TestActivityBatchImportFailureLeavesNoSource(t *testing.T) {
	s, b := batchFixture(t, "run")
	// Make the atomic destination directory unusable without changing a reader-visible batch.
	os.MkdirAll(s.Path("state", "activity"), 0o700)
	if err := os.WriteFile(s.Path("state", "activity", activityBatchDir), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if WriteActivityBatch(s, b.Source, b.Observations) == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	if _, err := os.Stat(s.Path("state", "activity", sourcesFile)); !os.IsNotExist(err) {
		t.Fatal("source registry changed")
	}
}

func TestActivityBatchDuplicateJSONKeysRefuseDespiteUnchangedContentDigest(t *testing.T) {
	s, b := batchFixture(t, "run")
	if err := WriteActivityBatch(s, b.Source, b.Observations); err != nil {
		t.Fatal(err)
	}
	file := s.Path("state", "activity", activityBatchDir, b.Source.SourceID+".json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate equal values still introduce an ambiguous committed representation.
	raw = []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1))
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSpoolChecked(s); err == nil {
		t.Fatal("duplicate JSON keys accepted")
	}
}
