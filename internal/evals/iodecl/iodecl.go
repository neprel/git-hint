// Package iodecl holds the B1 cli-file-v1 declaration (07B-J §14/§15): a declared command
// run once per case with exact stdin bytes. The eval builder and the container runner
// check a declaration with the same rules.
package iodecl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Interface is the only B1 interface.
const Interface = "cli-file-v1"

// Limits of a B1 declaration.
const (
	MaxCases       = 64
	MaxBytes       = 1 << 20
	MaxCaseSeconds = 30
	MaxEntryArgs   = 16
)

var caseID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Spec is tests/io/spec.json, written by the task author.
type Spec struct {
	Interface   string   `json:"interface"`
	Entry       []string `json:"entry"`
	Cases       []string `json:"cases"`
	CaseSeconds int      `json:"case_seconds"`
	Compare     string   `json:"compare"`
}

// Declaration is a checked declaration with its inputs and their digests.
type Declaration struct {
	Spec         Spec
	Inputs       map[string][]byte
	SpecDigest   string
	InputsDigest string
}

// Parse checks a declaration: the spec bytes and the inputs by case ID. Unknown fields, an
// interface or comparison other than the one, anything outside 1..64 unique safe case IDs,
// an unbounded entry or case limit, an input over 1 MiB, and inputs that are not exactly
// one per case are refused.
func Parse(spec []byte, inputs map[string][]byte) (*Declaration, error) {
	bad := func(why string) (*Declaration, error) {
		return nil, fmt.Errorf("the B1 declaration is invalid: %s", why)
	}
	if len(spec) > 64<<10 {
		return bad("spec too large")
	}
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return bad("spec is not one strict JSON object")
	}
	if s.Interface != Interface || s.Compare != "exact" {
		return bad("interface or comparison")
	}
	if len(s.Entry) == 0 || len(s.Entry) > MaxEntryArgs {
		return bad("entry")
	}
	for _, a := range s.Entry {
		if a == "" || len(a) > 256 || strings.ContainsRune(a, 0) {
			return bad("entry argument")
		}
	}
	if s.CaseSeconds < 1 || s.CaseSeconds > MaxCaseSeconds {
		return bad("case_seconds")
	}
	if len(s.Cases) == 0 || len(s.Cases) > MaxCases {
		return bad("number of cases")
	}
	if len(inputs) != len(s.Cases) {
		return bad("inputs are not exactly one per case")
	}
	d := &Declaration{Spec: s, Inputs: map[string][]byte{}}
	h := sha256.New()
	for _, id := range s.Cases {
		if !caseID.MatchString(id) {
			return bad("case ID")
		}
		if _, dup := d.Inputs[id]; dup {
			return bad("duplicate case ID")
		}
		in, ok := inputs[id]
		if !ok || len(in) > MaxBytes {
			return bad("input of a case")
		}
		d.Inputs[id] = in
		fmt.Fprintf(h, "%s\x00%d\x00", id, len(in))
		h.Write(in)
	}
	sum := sha256.Sum256(spec)
	d.SpecDigest = "sha256:" + hex.EncodeToString(sum[:])
	d.InputsDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return d, nil
}
