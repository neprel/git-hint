package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// Backend configuration record helper (controlled research setup; run on the machine that holds
// the store): the configuration record the server's mapper binds a run's ledgers to, built from
// the run's stored manifest (the same configuration digest the ledger writer uses) and the store
// key (the references); the key and the raw values never leave this process. The record is
// always ineligible: eligibility is a reviewed change of the committed record.
const (
	backendRecordHelper  = "hint-backend-record"
	backendRecordVersion = "0.1.0"
)

// backendInstance is the private file of the observed backend instance (OPS record): the
// profile, the weights identity and the instance values of the capture snapshot.
type backendInstance struct {
	ProfileID    string `json:"profile_id"`
	WeightsID    string `json:"weights_id"`
	ImageID      string `json:"image_id"`
	StartedAt    string `json:"started_at"`
	RestartCount *int64 `json:"restart_count"`
}

var (
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	profilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,127}$`)
	rfc3339Pattern = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,9})?(Z|[+-]\d\d:\d\d)$`)
)

func readBackendInstance(path string) (backendInstance, []byte, error) {
	var in backendInstance
	b, err := os.ReadFile(path)
	if err != nil {
		return in, nil, errors.New("the backend instance file is not readable")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var extra json.RawMessage
	if d.Decode(&in) != nil || d.Decode(&extra) != io.EOF || !profilePattern.MatchString(in.ProfileID) || !digestPattern.MatchString(in.WeightsID) ||
		!digestPattern.MatchString(in.ImageID) || !rfc3339Pattern.MatchString(in.StartedAt) || in.RestartCount == nil || *in.RestartCount < 0 {
		return backendInstance{}, nil, errors.New("the backend instance file is not usable")
	}
	return in, b, nil
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// backendRecord is one configuration record keyed by the run's configuration digest.
func backendRecord(s *store.Store, runID, expectFile, instanceFile string) (map[string]any, error) {
	m, err := workunits.ReadManifest(s, runID)
	if err != nil {
		return nil, errors.New("the run has no stored manifest")
	}
	cfg, err := ledgerConfiguration(s.Key(), m)
	if err != nil {
		return nil, err
	}
	e, err := readCaptureExpect(expectFile)
	if err != nil {
		return nil, err
	}
	expectRaw, _ := os.ReadFile(expectFile)
	in, instRaw, err := readBackendInstance(instanceFile)
	if err != nil {
		return nil, err
	}
	key := s.Key()
	return map[string]any{cfg.Digest(): map[string]any{
		"eligible": false, "profile_id": in.ProfileID, "weights_id": in.WeightsID,
		"backend": map[string]any{"image_id": in.ImageID, "started_at": in.StartedAt, "restart_count": *in.RestartCount,
			"host_ref": hint.DeriveID(key, "capture-host", e.Host), "container_ref": hint.DeriveID(key, "capture-container", e.Container),
			"served_model_ref": hint.DeriveID(key, "capture-served-model", e.ServedModel)},
		"provenance": map[string]any{"helper": backendRecordHelper, "version": backendRecordVersion,
			"inputs":        map[string]any{"expect_sha256": sha256Hex(expectRaw), "instance_sha256": sha256Hex(instRaw)},
			"configuration": "the run's stored work-unit manifest (ledgerConfiguration), digest as the ledger writer computes it"}}}, nil
}

// TestBackendConfigurationRecord writes the record for a real store and run: HINT_BACKEND_RECORD_STORE
// (the project directory), HINT_BACKEND_RECORD_RUN, HINT_LIVE_CAPTURE_EXPECT (raw OPS C3 values),
// HINT_BACKEND_RECORD_INSTANCE and HINT_BACKEND_RECORD_OUT (a new file). No request, no output of
// a raw value or the key.
func TestBackendConfigurationRecord(t *testing.T) {
	dir := os.Getenv("HINT_BACKEND_RECORD_STORE")
	if dir == "" {
		t.Skip("set HINT_BACKEND_RECORD_STORE")
	}
	out := os.Getenv("HINT_BACKEND_RECORD_OUT")
	if out == "" {
		t.Fatal("set HINT_BACKEND_RECORD_OUT")
	}
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := backendRecord(s, os.Getenv("HINT_BACKEND_RECORD_RUN"), os.Getenv("HINT_LIVE_CAPTURE_EXPECT"), os.Getenv("HINT_BACKEND_RECORD_INSTANCE"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("the output file exists or cannot be created")
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	t.Log("record written (references only)")
}

// The record's key is the configuration digest that the ledger writer binds the run's ledger to;
// its references are the store key's derivations of the expected raw values; it is ineligible
// and carries no raw value; unusable inputs are refused without naming a value.
func TestBackendRecordBindsTheWrittenLedger(t *testing.T) {
	s, l := validatedProject(t)
	complete := runner.LedgerReport{State: "complete", Counts: map[string]int{runner.EntryComplete: 1}, Components: map[string]int64{"prompt": 3, "completion": 2},
		ComponentsN: map[string]int{"prompt": 1, "completion": 1}, ModelCheck: "verified", Bypass: runner.BypassEnforced, Scope: runner.ScopeIsolatedContainer}
	m := workunits.Manifest{RunID: "jb-0123456789abcdef", Agent: "opencode", AgentVersion: "2.0.26", RunnerVersion: runner.DockerRunnerVersion,
		ModelLabel: "m", LaunchDigest: "sha256:" + strings.Repeat("d", 64), MaxRequests: 10, MaxSeconds: 900, Harness: "hint-docker", Attempts: 1}
	if err := workunits.Start(s, l, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	dir := filepath.Join(work, "jobs", "trial", "trial__1")
	os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(map[string]any{"hint_container": map[string]any{"ledger_sealed_at": "2026-10-20T12:00:00Z", "ledger": complete, "ledger_final": true}})
	os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
	if err := handOverLedger(s, work, m, "tr-0123456789abcdef", m.RunID, "ev-0123456789abcdef@2", 1, "ev-0123456789abcdef", "tf-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	obs, _, _ := project.ReadSpool(s)
	var written string
	for _, o := range obs {
		if o.ModelLedger != nil {
			written = o.ModelLedger.Binding.ConfigurationDigest
		}
	}
	files := t.TempDir()
	expect, instance := filepath.Join(files, "expect.json"), filepath.Join(files, "instance.json")
	os.WriteFile(expect, []byte(`{"host":"capture-host.example.invalid","container":"`+fullID+`","served_model":"example-model"}`), 0o600)
	inst := `{"profile_id":"example-profile","weights_id":"sha256:` + strings.Repeat("b", 64) + `","image_id":"sha256:` + strings.Repeat("e", 64) +
		`","started_at":"2026-01-02T03:04:05.000000006Z","restart_count":0}`
	os.WriteFile(instance, []byte(inst), 0o600)
	rec, err := backendRecord(s, m.RunID, expect, instance)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := rec[written].(map[string]any)
	if written == "" || len(rec) != 1 || !ok {
		t.Fatalf("not keyed by the written ledger's configuration digest %q: %v", written, rec)
	}
	be := r["backend"].(map[string]any)
	key := s.Key()
	if r["eligible"] != false || be["host_ref"] != hint.DeriveID(key, "capture-host", "capture-host.example.invalid") ||
		be["container_ref"] != hint.DeriveID(key, "capture-container", fullID) || be["served_model_ref"] != hint.DeriveID(key, "capture-served-model", "example-model") ||
		be["started_at"] != "2026-01-02T03:04:05.000000006Z" || be["restart_count"] != int64(0) {
		t.Fatalf("%v", r)
	}
	out, _ := json.Marshal(rec)
	for _, raw := range []string{"capture-host.example.invalid", fullID, `"example-model"`, hex.EncodeToString(key)} {
		if bytes.Contains(out, []byte(raw)) {
			t.Fatalf("raw value or key in the record")
		}
	}
	// unusable inputs
	if _, err := backendRecord(s, "jb-ffffffffffffffff", expect, instance); err == nil {
		t.Error("an unknown run was accepted")
	}
	for _, bad := range []string{`{}`, strings.Replace(inst, `"restart_count":0`, `"restart_count":-1`, 1), strings.Replace(inst, `"profile_id":"example-profile"`, `"profile_id":"Bad Profile"`, 1),
		strings.Replace(inst, `}`, `,"x":1}`, 1), inst + `{}`, strings.Replace(inst, `"started_at":"2026-01-02T03:04:05.000000006Z"`, `"started_at":"yesterday"`, 1)} {
		os.WriteFile(instance, []byte(bad), 0o600)
		if _, err := backendRecord(s, m.RunID, expect, instance); err == nil || strings.Contains(err.Error(), "example") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
