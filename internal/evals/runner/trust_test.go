package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMinTrust(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{nil, TrustUnknown},
		{[]string{TrustProcessSeparated}, TrustProcessSeparated},
		{[]string{TrustProcessSeparated, TrustSameProcess}, TrustSameProcess},
		{[]string{TrustProcessSeparated, TrustUnknown}, TrustUnknown},
		{[]string{TrustProcessSeparated, "trusted"}, TrustUnknown},
		{[]string{""}, TrustUnknown},
	} {
		if got := MinTrust(c.in...); got != c.want {
			t.Errorf("%v: %s", c.in, got)
		}
	}
}

// 07B-J §11.4(8): the trust of a trial covers the exact required check set; a missing
// required check gives unknown; values of other checks never raise it.
func TestTrustOver(t *testing.T) {
	tr := Trial{ResultTrust: TrustProcessSeparated, CheckTrust: map[string]string{"B1": TrustProcessSeparated, "C2": TrustSameProcess, "X": TrustProcessSeparated}}
	for _, c := range []struct {
		req  []string
		want string
	}{
		{[]string{"B1"}, TrustProcessSeparated},
		{[]string{"B1", "C2"}, TrustSameProcess},
		{[]string{"B1", "C9"}, TrustUnknown},
		{nil, TrustUnknown},
	} {
		if got := tr.TrustOver(c.req); got != c.want {
			t.Errorf("%v: %s", c.req, got)
		}
	}
	tr.ResultTrust = TrustSameProcess // the trial's own value caps the checks
	if tr.TrustOver([]string{"B1"}) != TrustSameProcess {
		t.Error("the trial value does not cap")
	}
	tr.TrustInvalid = true
	if tr.TrustOver([]string{"B1"}) != TrustUnknown {
		t.Error("an invalid statement counts")
	}
}

// ReadJob takes the trust only from the runner's hint_container: absent is unknown; an
// invalid value or a check named twice is a verifier error, never a pass.
func TestReadJobTrust(t *testing.T) {
	for _, c := range []struct {
		name, container string
		trust           string
		invalid         bool
	}{
		{"legacy", ``, TrustUnknown, false},
		{"runner", `,"hint_container":{"result_trust":"same_process","check_trust":{"C1":"same_process"}}`, TrustSameProcess, false},
		{"invalid-value", `,"hint_container":{"result_trust":"trusted"}`, TrustUnknown, true},
		{"number", `,"hint_container":{"result_trust":2}`, TrustUnknown, true},
		{"duplicate-check", `,"hint_container":{"result_trust":"same_process","check_trust":{"C1":"same_process","C1":"process_separated"}}`, TrustUnknown, true},
		{"check-not-object", `,"hint_container":{"check_trust":["C1"]}`, TrustUnknown, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			job := t.TempDir()
			dir := filepath.Join(job, "j__1")
			os.MkdirAll(filepath.Join(dir, "verifier"), 0o700)
			os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"trial_name":"j__1","verifier_result":{"rewards":{"reward":1}}`+c.container+`}`), 0o600)
			os.WriteFile(filepath.Join(dir, "verifier", "test-stdout.txt"), []byte("CHECK C1 PASS\n"), 0o600)
			ts, err := ReadJob(job)
			if err != nil || len(ts) != 1 {
				t.Fatal(ts, err)
			}
			tr := ts[0]
			if tr.ResultTrust != c.trust || tr.TrustInvalid != c.invalid {
				b, _ := json.Marshal(tr)
				t.Fatalf("%s", b)
			}
			if want := map[bool]string{true: "verifier_error", false: "passed"}[c.invalid]; tr.Outcome() != want {
				t.Fatalf("outcome %s", tr.Outcome())
			}
		})
	}
}
