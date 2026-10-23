package pipeline

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// liveSession is the observed result of one bounded §11 session (TestLiveOpenCodeTrial).
type liveSession struct {
	Trials            int
	Outcome           string // the trial outcome: passed, failed, timeout, ...
	Stop              string // the suite's stop reason ("" when the run was not cut)
	Requests          *int64 // forwarded model requests, retries included
	MaxRequests       int64
	Ledgers           int // ledger records handed over for the run
	LedgersNotWritten int
	LedgerState       string
	LedgerBypass      string
	LedgerReasons     []string
	CompleteResponses int // ledger entries with a 2xx response and known usage
	ReportErr         error
	KeyHits           []string // output and form of each key hit, never the value
	KeyScanIncomplete []string // parts the key scan could not read to the end
}

// verdict says whether the session verified the intended chain, so that the series may go on
// (macOS before Linux). A Go test PASS alone never allows the next session: the trial must
// pass, the model must have answered at least once with usage, the ledger must be one final,
// complete, enforced record, the report must build and a complete key scan must find nothing.
// Anything else stops the series with the actual result.
func (l liveSession) verdict() error {
	var errs []error
	fail := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if len(l.KeyHits) > 0 {
		fail("the model key appears in an output: %v", l.KeyHits)
	}
	if len(l.KeyScanIncomplete) > 0 {
		fail("the key scan is incomplete: %v", l.KeyScanIncomplete)
	}
	if l.Trials != 1 {
		fail("trials %d, want 1", l.Trials)
	}
	if l.Requests == nil || *l.Requests == 0 {
		fail("no forwarded model request")
	} else if *l.Requests > l.MaxRequests {
		fail("requests %d over the cap %d", *l.Requests, l.MaxRequests)
	}
	if l.Stop != "" {
		fail("the run stopped: %s", l.Stop)
	}
	if l.Outcome != "passed" {
		fail("trial outcome %q", l.Outcome)
	}
	if l.CompleteResponses == 0 {
		fail("no successful model response with usage")
	}
	if l.Ledgers != 1 || l.LedgersNotWritten != 0 {
		fail("ledger records %d, not written %d", l.Ledgers, l.LedgersNotWritten)
	}
	if l.LedgerState != "complete" || l.LedgerBypass != "enforced" || slices.Contains(l.LedgerReasons, "teardown_incomplete") {
		fail("ledger %s/%s %v", l.LedgerState, l.LedgerBypass, l.LedgerReasons)
	}
	if l.ReportErr != nil {
		fail("report: %v", l.ReportErr)
	}
	return errors.Join(errs...)
}

func TestLiveSessionVerdict(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	ok := liveSession{Trials: 1, Outcome: "passed", Requests: n(3), MaxRequests: 8, Ledgers: 1, LedgerState: "complete", LedgerBypass: "enforced", CompleteResponses: 3}
	if err := ok.verdict(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*liveSession){
		"zero requests":        func(l *liveSession) { l.Requests, l.CompleteResponses = n(0), 0 },
		"requests unknown":     func(l *liveSession) { l.Requests = nil },
		"over the cap":         func(l *liveSession) { l.Requests = n(9) },
		"budget exhausted":     func(l *liveSession) { l.Stop = "request_limit" },
		"failed trial":         func(l *liveSession) { l.Outcome = "failed" },
		"timeout":              func(l *liveSession) { l.Outcome = "timeout" },
		"provider failure":     func(l *liveSession) { l.CompleteResponses = 0 },
		"no ledger":            func(l *liveSession) { l.Ledgers = 0 },
		"ledger not written":   func(l *liveSession) { l.LedgersNotWritten = 1 },
		"ledger incomplete":    func(l *liveSession) { l.LedgerState = "incomplete" },
		"bypass not enforced":  func(l *liveSession) { l.LedgerBypass = "unverified" },
		"teardown incomplete":  func(l *liveSession) { l.LedgerReasons = []string{"teardown_incomplete"} },
		"report failed":        func(l *liveSession) { l.ReportErr = errors.New("x") },
		"key in an output":     func(l *liveSession) { l.KeyHits = []string{"stdout: raw"} },
		"key scan incomplete":  func(l *liveSession) { l.KeyScanIncomplete = []string{"run.tar: byte limit"} },
		"no trial":             func(l *liveSession) { l.Trials = 0 },
		"infrastructure error": func(l *liveSession) { l.Outcome = "infrastructure_error" },
	} {
		l := ok
		f(&l)
		if l.verdict() == nil {
			t.Errorf("%s: the series would continue", name)
		}
	}
}
