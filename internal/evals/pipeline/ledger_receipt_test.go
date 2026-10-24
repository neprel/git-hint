package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/activity"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/evals/workunits"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func receiptInput(t *testing.T, projectOrigin bool) (*store.Store, LedgerInput) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "main.go", "package main\n")
	testkit.Git(t, root, "add", "main.go")
	s, _, err := store.Init(root, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	in := LedgerInput{Trial: "trial", Run: "run", Eval: "eval@1", Attempt: 1, Task: "task", Family: "family", Agent: "opencode",
		SealedAt: "2026-10-24T12:00:00Z", Report: runner.LedgerReport{State: "complete", Counts: map[string]int{runner.EntryComplete: 1},
			Components: map[string]int64{"prompt": 3, "completion": 2}, ComponentsN: map[string]int{"prompt": 1, "completion": 1},
			ModelCheck: "verified", Bypass: runner.BypassEnforced, Scope: runner.ScopeIsolatedContainer}}
	if projectOrigin {
		now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		if _, err := project.Collect(s, project.CollectOptions{Now: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
		if _, err := project.WorkStart(s, "unit", nil, "", in.Family, &in.Configuration, now); err != nil {
			t.Fatal(err)
		}
		if err := project.WorkJoin(s, "unit", project.TaskID(s.Key(), in.Task), now); err != nil {
			t.Fatal(err)
		}
		u, err := project.LoadWork(s, "unit")
		if err != nil {
			t.Fatal(err)
		}
		in.Trial, in.Run, in.Eval, in.Attempt = "", "", "", 0
		in.Project = &ProjectRun{Label: "unit", Run: "project-run", AttemptRef: u.Attempts[0].Ref, ExecutionRef: hint.EvalRef(s.Key(), "execution", "unit", "project-run")}
	}
	return s, in
}

func checkPersistedReceipt(t *testing.T, s *store.Store, r *LedgerWriteReceipt) {
	t.Helper()
	if r == nil {
		t.Fatal("missing receipt")
	}
	obs, damage, err := project.ReadSpoolChecked(s)
	if err != nil || damage.Any() {
		t.Fatal(damage, err)
	}
	sources, err := project.Sources(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.ObservationID != r.ObservationID {
			continue
		}
		od, err := hint.Digest(o)
		if err != nil {
			t.Fatal(err)
		}
		src, ok := sources[o.SourceID]
		sd, err := hint.Digest(src)
		if err != nil || !ok || o.ModelLedger == nil {
			t.Fatal("receipt has no exact persisted source/ledger", err)
		}
		ld, err := hint.Digest(*o.ModelLedger)
		if err != nil {
			t.Fatal(err)
		}
		want := LedgerWriteReceipt{ObservationID: o.ObservationID, ObservationDigest: od, SourceID: o.SourceID, SourceDeclarationDigest: sd,
			LedgerRef: o.ModelLedger.LedgerRef, LedgerDigest: ld, Revision: o.ModelLedger.Revision, Binding: o.ModelLedger.Binding}
		if *r != want {
			t.Fatalf("receipt differs from original persisted emission: %+v != %+v", *r, want)
		}
		return
	}
	t.Fatal("receipt observation absent")
}

func receiptActivityBytes(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"sources.json", "spool.jsonl"} {
		b, err := os.ReadFile(s.Path("state", "activity", name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

func TestLedgerReceiptPersistedBindingReplayLateAndOwnership(t *testing.T) {
	for _, origin := range []string{"controlled", "project"} {
		t.Run(origin, func(t *testing.T) {
			s, in := receiptInput(t, origin == "project")
			status, r, err := WriteLedgerWithReceipt(s, in)
			if err != nil || status != "revision 0 written (complete)" {
				t.Fatal(status, err)
			}
			checkPersistedReceipt(t, s, r)
			if (r.Binding.Origin == "project") != (origin == "project") {
				t.Fatal("wrong binding origin")
			}
			first, before := *r, receiptActivityBytes(t, s)
			status, replay, err := WriteLedgerWithReceipt(s, in)
			if err != nil || status != "revision already written" || replay == r || *replay != first || !reflect.DeepEqual(before, receiptActivityBytes(t, s)) {
				t.Fatal("exact replay changed receipt or persisted bytes", status, err)
			}
			if status, err := WriteLedger(s, in); err != nil || status != "revision already written" {
				t.Fatal("status wrapper diverged", status, err)
			}
			original := in
			in.Revision = 1
			in.Report.Counts = maps.Clone(in.Report.Counts)
			in.Report.Counts["late"] = 1
			in.Report.State, in.Report.Reasons = "incomplete", []string{"late_traffic"}
			status, late, err := WriteLedgerWithReceipt(s, in)
			if err != nil || status != "revision 1 written (incomplete)" || late.Revision != 1 || late.LedgerRef != first.LedgerRef || late.LedgerDigest == first.LedgerDigest || *r != first {
				t.Fatal("late revision rewrote original receipt", status, err)
			}
			checkPersistedReceipt(t, s, r)
			checkPersistedReceipt(t, s, late)
			if status, earlier, err := WriteLedgerWithReceipt(s, original); err != nil || status != "revision already written" || *earlier != first {
				t.Fatal("replay selected or enriched from a later revision", status, err)
			}
			before = receiptActivityBytes(t, s)
			r.ObservationID, r.SourceID, r.Binding.RunRef, r.Binding.ConfigurationDigest, r.LedgerDigest, r.Revision = "changed", "changed", "changed", "changed", "changed", 90
			if *replay != first || !reflect.DeepEqual(before, receiptActivityBytes(t, s)) {
				t.Fatal("returned value shares mutable state")
			}
			// The account can change after the call without changing earlier receipts.
			in.Report.Counts["late"] = 99
			if *replay != first {
				t.Fatal("account mutation changed receipt")
			}
		})
	}
}

func TestLedgerReceiptSourceConflictRefusesBeforeWritesOrReplay(t *testing.T) {
	for _, phase := range []string{"initial", "replay", "late", "legacy-wrapper"} {
		t.Run(phase, func(t *testing.T) {
			s, in := receiptInput(t, false)
			if phase != "initial" && phase != "legacy-wrapper" {
				if _, _, err := WriteLedgerWithReceipt(s, in); err != nil {
					t.Fatal(err)
				}
			}
			src := activity.LedgerSource(s.Key(), in.Agent, in.Configuration)
			src.Agent = "conflicting-agent"
			if err := project.RegisterSource(s, src); err != nil {
				t.Fatal(err)
			}
			before := receiptActivityBytes(t, s)
			if phase == "late" {
				in.Revision, in.Report.State, in.Report.Reasons = 1, "incomplete", []string{"late_traffic"}
				in.Report.Counts["late"] = 1
			}
			if phase == "legacy-wrapper" {
				if status, err := WriteLedger(s, in); status != "" || !errors.Is(err, ErrLedger) {
					t.Fatal("legacy wrapper overwrote source", status, err)
				}
			} else if status, r, err := WriteLedgerWithReceipt(s, in); status != "" || r != nil || !errors.Is(err, ErrLedger) {
				t.Fatal("conflict returned provisional receipt", status, r, err)
			}
			if !reflect.DeepEqual(before, receiptActivityBytes(t, s)) {
				t.Fatal("source conflict changed persisted files")
			}
		})
	}
}

func TestLedgerReceiptMissingReplayDeclarationAndErrors(t *testing.T) {
	t.Run("missing replay declaration", func(t *testing.T) {
		s, in := receiptInput(t, false)
		if _, _, err := WriteLedgerWithReceipt(s, in); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(s.Path("state", "activity", "sources.json")); err != nil {
			t.Fatal(err)
		}
		before := receiptActivityBytes(t, s)
		if status, r, err := WriteLedgerWithReceipt(s, in); status != "" || r != nil || !errors.Is(err, ErrLedger) {
			t.Fatal("missing source gave replay receipt", status, r, err)
		}
		if !reflect.DeepEqual(before, receiptActivityBytes(t, s)) {
			t.Fatal("replay repaired missing declaration")
		}
	})
	t.Run("invalid account", func(t *testing.T) {
		s, in := receiptInput(t, false)
		in.Report.Counts[runner.EntryComplete] = -1
		if status, r, err := WriteLedgerWithReceipt(s, in); status != "" || r != nil || !errors.Is(err, ErrLedger) {
			t.Fatal("invalid account returned receipt", status, r, err)
		}
		if got := receiptActivityBytes(t, s); got["spool.jsonl"] != "" || got["sources.json"] != "" {
			t.Fatal("invalid account wrote state")
		}
	})
	t.Run("append write error", func(t *testing.T) {
		s, in := receiptInput(t, false)
		p := s.Path("state", "activity", "spool.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0400); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(p, 0600)
		// Do not claim a permission failure under a privileged test user.
		if f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0); err == nil {
			f.Close()
			t.Skip("test user bypasses file write permissions")
		}
		if status, r, err := WriteLedgerWithReceipt(s, in); status != "" || r != nil || err == nil {
			t.Fatal("failed append returned receipt", status, r, err)
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) != 0 {
			t.Fatal("failed append emitted observation", err)
		}
		sources, err := project.Sources(s)
		if err != nil || len(sources) != 1 {
			t.Fatal("test did not reach append after source registration", err)
		}
	})
}

func TestLedgerReceiptSuiteReportJSONGolden(t *testing.T) {
	r := SuiteReport{Suite: "suite", Trials: 1, Outcomes: map[string]int{"pass": 1}, LedgerReceipts: []LedgerWriteReceipt{{ObservationID: "not-public"}}}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"suite":"suite","trials":1,"outcomes":{"pass":1},"model_requests":null}`)
	if !bytes.Equal(got, want) {
		t.Fatalf("report wire changed: %s", got)
	}
}

func TestLedgerReceiptHandOverErrorsNeverReturnReceipt(t *testing.T) {
	for _, mode := range []string{"missing result", "malformed result", "missing seal"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := receiptInput(t, false)
			work := t.TempDir()
			if mode != "missing result" {
				dir := filepath.Join(work, "jobs", "trial", "trial__1")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				body := []byte(`{`)
				if mode == "missing seal" {
					body = []byte(`{"hint_container":{"ledger":{}}}`)
				}
				if err := os.WriteFile(filepath.Join(dir, "result.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if r, err := handOverLedgerWithReceipt(s, work, workunits.Manifest{}, "trial", "run", "eval@1", 1, "task", "family"); r != nil || err == nil {
				t.Fatal("failed hand-over returned receipt", r, err)
			}
			if got := receiptActivityBytes(t, s); got["spool.jsonl"] != "" || got["sources.json"] != "" {
				t.Fatal("failed hand-over wrote state")
			}
		})
	}
}
