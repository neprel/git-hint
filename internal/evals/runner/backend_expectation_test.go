package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/hint"
)

func expectationJSON(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(hostLine(nil), &m); err != nil {
		t.Fatal(err)
	}
	m["capture_target"], m["capture_container"] = "capture-host.example.invalid", "example-model-container"
	if edit != nil {
		edit(m)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestBackendExpectationUsesCommonReferencesAndTransport(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	e, err := ParseBackendExpectation(expectationJSON(t, nil), key)
	if err != nil {
		t.Fatal(err)
	}
	if e.Capture == nil || e.Snapshot == nil || !completeExpectedBackend(e.Snapshot) {
		t.Fatal("complete metadata was not bound")
	}
	if e.Snapshot.HostRef != hint.DeriveID(key, "capture-host", "capture-host.example.invalid") ||
		e.Snapshot.ContainerRef != hint.DeriveID(key, "capture-container", "c0ffeec0ffee") ||
		*e.Snapshot.Flags.ServedModelRef != hint.DeriveID(key, "capture-served-model", "example-model") {
		t.Fatal("reference domains differ from the common capture")
	}
	_, ref := (RunOptions{BackendCapture: e.Capture, CaptureKey: key}).captureOptions()
	if ref("capture-host", "capture-host.example.invalid") != e.Snapshot.HostRef {
		t.Fatal("normal capture and expectation differ")
	}
	b, _ := json.Marshal(e.Snapshot)
	for _, private := range []string{"capture-host.example.invalid", "c0ffeec0ffee", "example-model"} {
		if strings.Contains(string(b), private) {
			t.Fatal("raw backend name in snapshot")
		}
	}
	key[0] = 'x'
	other, err := ParseBackendExpectation(expectationJSON(t, nil), key)
	if err != nil || other.Snapshot.HostRef == e.Snapshot.HostRef {
		t.Fatal("the actual key was not used")
	}
}

func TestBackendExpectationFalseIsObservedNotMissing(t *testing.T) {
	e, err := ParseBackendExpectation(expectationJSON(t, func(m map[string]any) {
		m["prompt_tokens_details"], m["prefix_caching"], m["restart_count"] = false, false, 0
	}), []byte("synthetic-key"))
	if err != nil || e.Snapshot.Flags.PromptTokensDetails == nil || *e.Snapshot.Flags.PromptTokensDetails ||
		e.Snapshot.Flags.PrefixCaching == nil || *e.Snapshot.Flags.PrefixCaching || *e.Snapshot.RestartCount != 0 {
		t.Fatal("reported false/zero facts were changed into missing or true facts")
	}
}

func TestBackendExpectationRejectsMalformedMissingAndPrivateFields(t *testing.T) {
	good := expectationJSON(t, nil)
	for name, data := range map[string][]byte{
		"duplicate":    append([]byte(`{"host":"decoy",`), good[1:]...),
		"trailing":     append(append([]byte{}, good...), []byte(`{}`)...),
		"oversized":    []byte(strings.Repeat(" ", MaxBackendExpectationBytes+1)),
		"empty":        nil,
		"invalid UTF8": []byte("{\"host\":\"\xff\"}"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBackendExpectation(data, []byte("key")); err == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
	for name, edit := range map[string]func(map[string]any){
		"secret":            func(m map[string]any) { m["api_key"] = "sentinel-secret" },
		"provider":          func(m map[string]any) { m["provider"] = "forbidden" },
		"missing":           func(m map[string]any) { delete(m, "prefix_caching") },
		"null flag":         func(m map[string]any) { m["prefix_caching"] = nil },
		"null time":         func(m map[string]any) { m["started_at"] = nil },
		"bool count":        func(m map[string]any) { m["restart_count"] = false },
		"string flag":       func(m map[string]any) { m["prefix_caching"] = "true" },
		"unknown parser":    func(m map[string]any) { m["reasoning_parser"] = "unknown-parser" },
		"invalid target":    func(m map[string]any) { m["capture_target"] = "-oProxyCommand=bad" },
		"invalid container": func(m map[string]any) { m["capture_container"] = "c;id" },
		"missing served":    func(m map[string]any) { m["served_model"] = nil },
		"bad time":          func(m map[string]any) { m["started_at"] = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBackendExpectation(expectationJSON(t, edit), []byte("key")); err == nil || strings.Contains(err.Error(), "sentinel-secret") {
				t.Fatal("invalid metadata accepted or disclosed")
			}
		})
	}
	if _, err := ParseBackendExpectation(good, nil); err == nil {
		t.Fatal("missing reference key accepted")
	}
}
