package runjournal

import (
	"encoding/json"
	"github.com/neprel/git-hint/internal/hint"
)

// Revise appends a revision when the reported form changed (state or attempts).
func (u *Unit) Revise() error {
	scopeDigest, err := hint.Digest(plain(u.Scope))
	if err != nil {
		return err
	}
	var prev any
	if n := len(u.Revisions); n > 0 {
		last := u.Revisions[n-1]
		b1, _ := json.Marshal(last["attempts"])
		b2, _ := json.Marshal(plain(u.Attempts))
		if last["state"] == u.State && string(b1) == string(b2) {
			return nil
		}
		prev = last["revision_digest"]
	}
	r := map[string]any{"unit_ref": u.UnitRef, "origin": "controlled", "scope": u.Scope, "scope_digest": scopeDigest, "revision": len(u.Revisions) + 1,
		"predecessor": prev, "state": u.State, "attempts": u.Attempts, "declared_by": "user"}
	d, err := hint.Digest(plain(r))
	if err != nil {
		return err
	}
	r["revision_digest"] = d
	u.Revisions = append(u.Revisions, plain(r).(map[string]any))
	return nil
}

func plain(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
