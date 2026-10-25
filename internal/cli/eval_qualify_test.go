package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvalQualifyAliasPolicyRefusesBeforeEngine(t *testing.T) {
	shop, _ := setup(t)
	must(t, run(t, shop, "", "eval", "on"))
	for _, alias := range []string{"qualify", "__qualify"} {
		r := run(t, shop, "", "eval", alias)
		if r.code == 0 || !strings.Contains(r.stderr, "code execution is off") {
			t.Fatalf("%s did not refuse execution-off policy: %s", alias, r.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(shop, ".hint/state/evals/worker.lock")); !os.IsNotExist(err) {
		t.Fatal("refused qualification created a worker lock")
	}
}

func TestEvalQualifyHelpAndExtraArgumentsBeforeStore(t *testing.T) {
	dir := t.TempDir()
	for _, alias := range []string{"qualify", "__qualify"} {
		for _, arg := range []string{"-h", "unexpected"} {
			r := run(t, dir, "", "eval", alias, arg)
			if r.code != 2 || strings.Contains(r.stderr, "not initialized") {
				t.Fatalf("%s %s reached Store: %+v", alias, arg, r)
			}
		}
	}
}

func TestEvalQualifyOriginalDeadlineAndCancellation(t *testing.T) {
	entered := time.Now().Add(-30 * time.Second)
	work, total, cancel := evalContext(entered, 660*time.Second, 60*time.Second)
	defer cancel()
	w, _ := work.Deadline()
	f, _ := total.Deadline()
	if !w.Equal(entered.Add(600*time.Second)) || !f.Equal(entered.Add(660*time.Second)) {
		t.Fatal("qualification renewed its original budget")
	}
	cancel()
	if work.Err() == nil || total.Err() == nil {
		t.Fatal("cancellation did not reach both contexts")
	}
	late, whole, stop := evalContext(time.Now().Add(-700*time.Second), 660*time.Second, 60*time.Second)
	defer stop()
	if late.Err() == nil || whole.Err() == nil {
		t.Fatal("late entry acquired a fresh qualification budget")
	}
}
