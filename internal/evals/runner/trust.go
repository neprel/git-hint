package runner

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Result trust (07B-J §11-§12): how far a verdict is independent of the candidate code.
// same_process: the checks ran inside the process of the code under test (the generated
// verifier, gate-v1); process_separated: the verdict came from an evaluator that never
// loaded or executed candidate code. unknown: no runner statement (legacy results, Harbor
// jobs) or anything incomplete. Only the runner writes the value; nothing raises it.
const (
	TrustUnknown          = "unknown"
	TrustSameProcess      = "same_process"
	TrustProcessSeparated = "process_separated"
)

var trustRank = map[string]int{TrustUnknown: 0, TrustSameProcess: 1, TrustProcessSeparated: 2}

// ValidTrust says whether v is one of the three values.
func ValidTrust(v string) bool { _, ok := trustRank[v]; return ok }

// MinTrust is the lowest of the values; no value, or an invalid one, gives unknown.
func MinTrust(vs ...string) string {
	if len(vs) == 0 {
		return TrustUnknown
	}
	low := TrustProcessSeparated
	for _, v := range vs {
		r, ok := trustRank[v]
		if !ok {
			return TrustUnknown
		}
		if r < trustRank[low] {
			low = v
		}
	}
	return low
}

// TrustOver is the trust of a trial over the exact required check set: the minimum of the
// trial's value and the value of every required check. A missing required check, an empty
// required set or an invalid statement gives unknown; values of other checks cannot raise
// it.
func (t Trial) TrustOver(required []string) string {
	if t.TrustInvalid || len(required) == 0 {
		return TrustUnknown
	}
	vs := []string{t.ResultTrust}
	for _, c := range required {
		v, ok := t.CheckTrust[c]
		if !ok {
			return TrustUnknown
		}
		vs = append(vs, v)
	}
	return MinTrust(vs...)
}

var errTrust = errors.New("invalid result trust")

// readTrust reads the runner's trust statement of a trial result (hint_container). No
// statement is unknown; a value outside the three, a non-object check_trust or a check
// named twice is invalid.
func readTrust(raw json.RawMessage) (string, map[string]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return TrustUnknown, nil, nil
	}
	var c map[string]json.RawMessage
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", nil, errTrust
	}
	rt, ct := TrustUnknown, map[string]string(nil)
	if v, ok := c["result_trust"]; ok {
		if json.Unmarshal(v, &rt) != nil || !ValidTrust(rt) {
			return "", nil, errTrust
		}
	}
	if v, ok := c["check_trust"]; ok {
		dec := json.NewDecoder(bytes.NewReader(v))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return "", nil, errTrust
		}
		ct = map[string]string{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return "", nil, errTrust
			}
			var val string
			if dec.Decode(&val) != nil || !ValidTrust(val) {
				return "", nil, errTrust
			}
			if _, dup := ct[k.(string)]; dup {
				return "", nil, errTrust
			}
			ct[k.(string)] = val
		}
	}
	return rt, ct, nil
}

// RequiredChecks is the exact required check set of an eval record: the check IDs of its
// verifier, in order.
func RequiredChecks(rec map[string]any) []string {
	v, _ := rec["verifier"].(map[string]any)
	cs, _ := v["checks"].([]any)
	var out []string
	for _, c := range cs {
		if m, _ := c.(map[string]any); m != nil {
			if id, _ := m["check_id"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}
