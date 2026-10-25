package cli

// This formatter reads an already saved ordinary server response. It performs
// no request, store lookup, usage mapping, comparison, effect or forecast.
import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const receivedCompareMaxBytes = 1 << 20

var errReceivedCompare = errors.New("received comparison file is invalid or exceeds its limit")

func evalCompareReceived(e *Env, path string) int {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.Dir, path)
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > receivedCompareMaxBytes {
		return fail(e, errReceivedCompare)
	}
	// Nonblocking open also refuses a FIFO substituted after the regular-file check.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(e, errReceivedCompare)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fail(e, errReceivedCompare)
	}
	data, err := io.ReadAll(io.LimitReader(f, receivedCompareMaxBytes+1))
	if err != nil || len(data) > receivedCompareMaxBytes {
		return fail(e, errReceivedCompare)
	}
	after, err := f.Stat()
	current, cerr := os.Lstat(path)
	if err != nil || cerr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || after.Mode() != opened.Mode() {
		return fail(e, errReceivedCompare)
	}
	text, err := formatReceivedComparison(data)
	if err != nil {
		return fail(e, err)
	}
	fmt.Fprint(e.Stdout, text)
	return 0
}

// Only the three consumed object levels are decoded here. Duplicate critical
// fields cannot override an earlier unknown label. No new schema/parser API.
func receivedCompareFields(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errReceivedCompare
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		t, err = d.Token()
		key, ok := t.(string)
		if err != nil || !ok || len(out) >= 64 {
			return nil, errReceivedCompare
		}
		if _, dup := out[key]; dup {
			return nil, errReceivedCompare
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, errReceivedCompare
		}
		out[key] = v
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return nil, errReceivedCompare
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errReceivedCompare
	}
	return out, nil
}
func receivedCompareString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
func receivedCompareInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || len(raw) > 64 {
		return 0, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := string(n)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, e := strconv.Atoi(s[i+1:])
		if e != nil || exp < -18 || exp > 18 {
			return 0, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return r.Num().Int64(), true
}
func formatReceivedComparison(data []byte) (string, error) {
	if len(data) > receivedCompareMaxBytes {
		return "", errReceivedCompare
	}
	root, err := receivedCompareFields(data)
	if err != nil {
		return "", err
	}
	raw := bytes.TrimSpace(root["groups"])
	if len(raw) == 0 || raw[0] != '[' {
		return "", errReceivedCompare
	}
	var groups []json.RawMessage
	if json.Unmarshal(raw, &groups) != nil || len(groups) > 1000 {
		return "", errReceivedCompare
	}
	var out strings.Builder
	out.WriteString("Observed received usage\n")
	if len(groups) == 0 {
		out.WriteString("No received comparison groups.\n")
	}
	for i, raw := range groups {
		g, e := receivedCompareFields(raw)
		if e != nil {
			return "", e
		}
		attempts, attemptsOK := receivedCompareInteger(g["attempts"])
		attemptText := "unknown"
		if attemptsOK {
			attemptText = strconv.FormatInt(attempts, 10)
		}
		totalText := "unknown"
		tokenRaw := bytes.TrimSpace(g["tokens"])
		if len(tokenRaw) > 0 && tokenRaw[0] == '{' {
			tok, e := receivedCompareFields(tokenRaw)
			if e != nil {
				return "", e
			}
			total, totalOK := receivedCompareInteger(tok["total"])
			known, knownOK := receivedCompareInteger(tok["known"])
			var coverage []json.RawMessage
			coverageOK := json.Unmarshal(tok["coverage"], &coverage) == nil && len(coverage) == 2
			var covered, all int64
			var coveredOK, allOK bool
			if coverageOK {
				covered, coveredOK = receivedCompareInteger(coverage[0])
				all, allOK = receivedCompareInteger(coverage[1])
			}
			if receivedCompareString(g["label"]) == "observed" && receivedCompareString(g["counter_semantics"]) == "hint-task-tokens-v1" && receivedCompareString(g["counter_scope"]) == "full_task" && attemptsOK && attempts > 0 && totalOK && knownOK && known == total && coveredOK && allOK && covered == attempts && all == attempts {
				totalText = strconv.FormatInt(total, 10)
			}
		}
		fmt.Fprintf(&out, "Group %d: attempts=%s; canonical full-task tokens=%s (uncached input + output)\n", i+1, attemptText, totalText)
	}
	return out.String(), nil
}
