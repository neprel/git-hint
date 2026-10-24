package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
)

func TestLiveCaptureOptIn(t *testing.T) {
	for _, c := range []struct {
		target, container string
		on, fail          bool
	}{{"", "", false, false}, {"capture-target.example.invalid", "", false, true}, {"", "example-model-container", false, true},
		{"capture-target.example.invalid", "example-model-container", true, false}, {"-oProxyCommand=x", "example-model-container", false, true}, {"capture-target.example.invalid", "c;id", false, true}} {
		t.Setenv("HINT_LIVE_CAPTURE_SSH", c.target)
		t.Setenv("HINT_LIVE_CAPTURE_CONTAINER", c.container)
		got, err := liveCapture()
		if (err != nil) != c.fail || (got != nil) != c.on {
			t.Errorf("%+v: %v %v", c, got, err)
		}
	}
}

func captureLedger(host, ctr, served string, endStatus string) *activity.ModelLedger {
	snap := func() *activity.LedgerCaptureSnapshot {
		return &activity.LedgerCaptureSnapshot{ImageID: "sha256:" + strings.Repeat("e", 64), HostRef: host, ContainerRef: ctr, Flags: activity.LedgerFlags{ServedModelRef: &served}}
	}
	return &activity.ModelLedger{BackendCapture: &activity.LedgerBackendCapture{Method: "runner_bracket_v1",
		Start: activity.LedgerCapture{Status: "complete", Snapshot: snap()}, End: activity.LedgerCapture{Status: endStatus, Snapshot: snap()}}}
}

const fullID = "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"

func TestCaptureCheckBooleansOnly(t *testing.T) {
	key := []byte("store-key")
	dir := t.TempDir()
	expect := filepath.Join(dir, "expect.json")
	os.WriteFile(expect, []byte(`{"host":"capture-host.example.invalid","container":"c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff","served_model":"example-model"}`), 0o600)
	h, c, m := hint.DeriveID(key, "capture-host", "capture-host.example.invalid"), hint.DeriveID(key, "capture-container", fullID), hint.DeriveID(key, "capture-served-model", "example-model")
	want := []string{"complete", "equal_snapshots", "host_ref_equal", "container_ref_equal", "served_model_ref_equal"}
	got, raw, err := captureCheck(key, []*activity.ModelLedger{captureLedger(h, c, m, "complete")}, expect)
	if err != nil || got["state"] != "checked" || !slices.Equal(raw, []string{"capture-host.example.invalid", fullID}) {
		t.Fatal(got, raw, err)
	}
	for _, k := range want {
		if got[k] != true {
			t.Errorf("%s: %v", k, got[k])
		}
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "capture-host.example.invalid") || strings.Contains(string(b), fullID) || strings.Contains(string(b), h) {
		t.Fatalf("raw value or reference in the check: %s", b)
	}
	for name, c := range map[string]struct {
		led []*activity.ModelLedger
		key string
	}{
		"other host":      {[]*activity.ModelLedger{captureLedger(hint.DeriveID(key, "capture-host", "other"), c, m, "complete")}, "host_ref_equal"},
		"other container": {[]*activity.ModelLedger{captureLedger(h, hint.DeriveID(key, "capture-container", "x"), m, "complete")}, "container_ref_equal"},
		"other model":     {[]*activity.ModelLedger{captureLedger(h, c, hint.DeriveID(key, "capture-served-model", "x"), "complete")}, "served_model_ref_equal"},
		"other key":       {[]*activity.ModelLedger{captureLedger(hint.DeriveID([]byte("k2"), "capture-host", "capture-host.example.invalid"), c, m, "complete")}, "host_ref_equal"},
		"end timeout":     {[]*activity.ModelLedger{captureLedger(h, c, m, "timeout")}, "complete"},
		"no ledger":       {nil, "complete"},
		"no capture":      {[]*activity.ModelLedger{{}}, "complete"},
	} {
		got, _, err := captureCheck(key, c.led, expect)
		if err != nil || got[c.key] != false {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if got, raw, err := captureCheck(key, nil, ""); err != nil || got["state"] != "not_checked" || raw != nil {
		t.Fatal("no expect file is not_checked")
	}
	for _, bad := range []string{`{}`, `{"host":"a","container":"b"}`, `{"host":"a","container":"b","served_model":"c","x":1}`, `nope`,
		`{"host":"capture-host.example.invalid","container":"c0ffeec0ffee…c0ffc0ff","served_model":"example-model"}`, // a shortened record value
		`{"host":"capture-host.example.invalid","container":"c0ffeec0ffee","served_model":"example-model"}`,
		`{"host":"capture-host.example.invalid","container":"` + strings.ToUpper(fullID) + `","served_model":"example-model"}`,
		`{"host":"capture-host.example.invalid","container":"` + fullID + `","served_model":"example-model"} {}`, // more than one object
		`{"host":"capture-host.example.invalid","container":"` + fullID + `","served_model":"example-model"}{"host":"x"}`,
		`{"host":"capture-host.example.invalid","container":"` + fullID + `","served_model":"example-model"}}`, // an unmatched delimiter
		`{"host":"capture-host.example.invalid","container":"` + fullID + `","served_model":"example-model"}]`,
		`{"host":"capture-host.example.invalid","container":"` + fullID + `","served_model":"example-model"} x`} {
		os.WriteFile(expect, []byte(bad), 0o600)
		_, _, err := captureCheck(key, nil, expect)
		if err == nil || strings.Contains(err.Error(), "capture-host.example.invalid") || strings.Contains(err.Error(), "c0ffee") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, _, err := captureCheck(key, nil, filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestCapturePreflight(t *testing.T) {
	expect := filepath.Join(t.TempDir(), "expect.json")
	os.WriteFile(expect, []byte(`{"host":"capture-host.example.invalid","container":"`+fullID+`","served_model":"example-model"}`+"\n"), 0o600)
	c, _ := runner.NewSSHCapture("capture-target.example.invalid", "example-model-container")
	if capturePreflight([]byte("k"), nil, "") != nil || capturePreflight([]byte("k"), nil, expect) != nil {
		t.Fatal("the no-capture path needs nothing")
	}
	if capturePreflight([]byte("k"), c, "") == nil {
		t.Fatal("captures without an expect file passed")
	}
	if err := capturePreflight([]byte("k"), c, expect); err != nil {
		t.Fatal(err)
	}
	if capturePreflight([]byte("k"), c, expect+".missing") == nil {
		t.Fatal("a missing expect file passed")
	}
}

func TestLiveOutputScanCoversRawIdentifiers(t *testing.T) {
	dir := t.TempDir()
	expect := filepath.Join(dir, "expect.json")
	os.WriteFile(expect, []byte(`{"host":"capture-host.example.invalid","container":"`+fullID+`","served_model":"example-model"}`), 0o600)
	out := filepath.Join(dir, "out")
	os.Mkdir(out, 0o700)
	os.WriteFile(filepath.Join(out, "summary.json"), []byte(`{"model":"example-model","note":"clean"}`), 0o600)
	r, err := liveOutputScan([]string{out}, "synthetic-key-123", "example-model", expect)
	if err != nil || !r.expect || r.files != 1 || r.keyHits != nil || r.rawHits != nil || r.rawIncomplete != nil {
		t.Fatalf("%+v %v", r, err)
	}
	// the served name is the declared model (exported on purpose); the host and the container ID
	// are found, and the hit never shows the value
	for name, body := range map[string]string{
		"host.txt":      "seen capture-host.example.invalid here",
		"container.txt": "id " + fullID,
		"escaped.json":  `{"c":"` + fullID + `"}`,
	} {
		d := filepath.Join(t.TempDir(), "out")
		os.Mkdir(d, 0o700)
		os.WriteFile(filepath.Join(d, name), []byte(body), 0o600)
		r, err := liveOutputScan([]string{d}, "synthetic-key-123", "example-model", expect)
		if err != nil || r.rawHits == nil {
			t.Errorf("%s: no raw hit (%+v %v)", name, r, err)
		}
		if strings.Contains(strings.Join(r.rawHits, " "), fullID) || strings.Contains(strings.Join(r.rawHits, " "), "capture-host.example.invalid") {
			t.Errorf("%s: a hit shows the value", name)
		}
	}
	if r, err := liveOutputScan([]string{out}, "synthetic-key-123", "example-model", ""); err != nil || r.expect || r.rawHits != nil {
		t.Fatal("without an expect file there is no raw scan")
	}
	os.WriteFile(expect, []byte(`{"host":"h","container":"short","served_model":"m"}`), 0o600)
	if _, err := liveOutputScan([]string{out}, "synthetic-key-123", "example-model", expect); err == nil {
		t.Fatal("an unusable expect file was accepted")
	}
}
