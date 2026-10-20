package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/neprel/git-hint/internal/evals/pipeline"
)

// adapterLedger writes the USAGE record of one ledger revision (draft v0.2) to the activity
// spool (pipeline.WriteLedger): it refuses an inconsistent account, a revision that is not the
// next one, a second body for a written revision, and a changed binding or seal time; a
// replay of a written revision writes nothing.
func adapterLedger(e *Env, args []string) int {
	fs := flags(e, "adapter ledger", "reads one sealed model ledger on stdin; writes local state; no network")
	if fs.Parse(args) != nil {
		return 2
	}
	s, err := open(e)
	if err != nil {
		return fail(e, err)
	}
	b, err := io.ReadAll(io.LimitReader(e.Stdin, 1<<20))
	if err != nil {
		return fail(e, err)
	}
	var in pipeline.LedgerInput
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fail(e, fmt.Errorf("the ledger input is not readable: %w; nothing was written", err))
	}
	msg, err := pipeline.WriteLedger(s, in)
	if err != nil {
		return fail(e, fmt.Errorf("%v; nothing was written", err))
	}
	fmt.Fprintln(e.Stdout, "adapter ledger: "+msg)
	return 0
}
