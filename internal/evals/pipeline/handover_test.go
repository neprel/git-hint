package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/project"
)

// 07B-J §23: the container runner's ledger is handed over through the shared writer; a ledger
// that is not final, or whose finality is not stated, is published incomplete with
// teardown_incomplete even when its own account is complete; no ledger writes nothing; a
// replay writes nothing more.
func TestLedgerHandOver(t *testing.T) {
	complete := runner.LedgerReport{State: "complete", Counts: map[string]int{runner.EntryComplete: 1}, Components: map[string]int64{"prompt": 3, "completion": 2},
		ComponentsN: map[string]int{"prompt": 1, "completion": 1}, ModelCheck: "verified", Bypass: runner.BypassEnforced, Scope: runner.ScopeIsolatedContainer}
	m := workunits.Manifest{RunID: "jb-0123456789abcdef", Agent: "opencode", AgentVersion: "2.0.26", RunnerVersion: runner.DockerRunnerVersion,
		ModelLabel: "m", LaunchDigest: "sha256:" + string(make([]byte, 0)), MaxRequests: 10, MaxSeconds: 900, Harness: "hint-docker"}
	yes, no := true, false
	for _, c := range []struct {
		name   string
		final  *bool
		ledger *runner.LedgerReport
		state  string // "" = no record
	}{
		{"final", &yes, &complete, "complete"},
		{"not final", &no, &complete, "incomplete"},
		{"finality missing", nil, &complete, "incomplete"},
		{"no ledger", &yes, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := e2eProject(t)
			work := t.TempDir()
			dir := filepath.Join(work, "jobs", "trial", "trial__1")
			os.MkdirAll(dir, 0o700)
			hc := map[string]any{"ledger_sealed_at": "2026-10-20T12:00:00Z"}
			if c.ledger != nil {
				hc["ledger"] = c.ledger
			}
			if c.final != nil {
				hc["ledger_final"] = *c.final
			}
			b, _ := json.Marshal(map[string]any{"hint_container": hc})
			os.WriteFile(filepath.Join(dir, "result.json"), b, 0o600)
			var firstReceipt *LedgerWriteReceipt
			for i := 0; i < 2; i++ { // the second hand-over is a replay
				receipt, err := handOverLedgerWithReceipt(s, work, m, "tr-0123456789abcdef", m.RunID, "ev-0123456789abcdef@2", 1, "ev-0123456789abcdef", "tf-0123456789abcdef")
				if err != nil {
					t.Fatal(err)
				}
				if (receipt == nil) != (c.ledger == nil) {
					t.Fatal("missing ledger invented a receipt, or written ledger lost it")
				}
				if receipt != nil {
					checkPersistedReceipt(t, s, receipt)
					if firstReceipt != nil && *receipt != *firstReceipt {
						t.Fatal("hand-over replay changed receipt")
					}
					firstReceipt = receipt
				}
			}
			if err := handOverLedger(s, work, m, "tr-0123456789abcdef", m.RunID, "ev-0123456789abcdef@2", 1, "ev-0123456789abcdef", "tf-0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
			obs, _, err := project.ReadSpool(s)
			if err != nil {
				t.Fatal(err)
			}
			var records []string
			for _, o := range obs {
				if o.ModelLedger != nil {
					records = append(records, o.ModelLedger.State)
					if o.ModelLedger.State == "incomplete" {
						found := false
						for _, r := range o.ModelLedger.Reasons {
							found = found || r == "teardown_incomplete"
						}
						if !found {
							t.Fatalf("reasons %v", o.ModelLedger.Reasons)
						}
					}
				}
			}
			switch {
			case c.state == "" && len(records) != 0, c.state != "" && (len(records) != 1 || records[0] != c.state):
				t.Fatalf("records %v, want %q once", records, c.state)
			}
		})
	}
}
