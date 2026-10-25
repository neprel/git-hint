package trees

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/hint"
)

const selectedWireKind = "git-selected-receipt-wire-v1"
const sourceWireKind = "git-source-receipt-wire-v1"
const receiptWireMax = 8 << 20
const receiptJSONNodes = 250000

type SelectedSourceWire struct{ SelectedA, SelectedB, SourceA, SourceB []byte }
type selectedWireEnvelope struct {
	Kind    string          `json:"kind"`
	Receipt SelectedReceipt `json:"receipt"`
}
type sourceWireEnvelope struct {
	Kind    string            `json:"kind"`
	Receipt SourceViewReceipt `json:"receipt"`
}

// DecodedSelectedSourcePair holds copied consistency observations, not an origin,
// authority, clock, start, or acceptance capability. It never opens referenced paths.
type DecodedSelectedSourcePair struct {
	limits Limits
	a, b   SelectedReceipt
	x, y   SourceViewReceipt
	refs   []sourceObjectRequirement
}

func EncodeSelectedSourcePair(limits Limits, a, b SelectedReceipt, x, y SourceViewReceipt) (SelectedSourceWire, error) {
	var zero SelectedSourceWire
	if ValidateSelectedSourcePair(limits, a, b, x, y) != nil || !receiptShapesMatch() {
		return zero, codecProblem()
	}
	values := []any{selectedWireEnvelope{selectedWireKind, a}, selectedWireEnvelope{selectedWireKind, b}, sourceWireEnvelope{sourceWireKind, x}, sourceWireEnvelope{sourceWireKind, y}}
	raw := make([][]byte, 4)
	for i, v := range values {
		b, e := hint.Canonical(v)
		if e != nil || len(b) > receiptWireMax {
			return zero, codecProblem()
		}
		raw[i] = b
	}
	wire := SelectedSourceWire{raw[0], raw[1], raw[2], raw[3]}
	if _, e := DecodeSelectedSourcePair(limits, wire); e != nil {
		return zero, e
	}
	return wire, nil
}
func DecodeSelectedSourcePair(limits Limits, wire SelectedSourceWire) (*DecodedSelectedSourcePair, error) {
	var a, b selectedWireEnvelope
	var x, y sourceWireEnvelope
	for _, v := range []struct {
		raw    []byte
		target any
	}{{wire.SelectedA, &a}, {wire.SelectedB, &b}, {wire.SourceA, &x}, {wire.SourceB, &y}} {
		if decodeReceiptJSON(v.raw, v.target, true) != nil {
			return nil, codecProblem()
		}
	}
	if a.Kind != selectedWireKind || b.Kind != selectedWireKind || x.Kind != sourceWireKind || y.Kind != sourceWireKind || ValidateSelectedSourcePair(limits, a.Receipt, b.Receipt, x.Receipt, y.Receipt) != nil {
		return nil, codecProblem()
	}
	lim, _ := ResolveStrictCaptureLimits(limits)
	p := &DecodedSelectedSourcePair{limits: lim, a: a.Receipt, b: b.Receipt, x: x.Receipt, y: y.Receipt}
	refs, e := p.objectRequirements()
	if e != nil {
		return nil, e
	}
	p.refs = refs
	return p, nil
}
func (p *DecodedSelectedSourcePair) Receipts() (SelectedReceipt, SelectedReceipt, SourceViewReceipt, SourceViewReceipt) {
	if p == nil {
		return SelectedReceipt{}, SelectedReceipt{}, SourceViewReceipt{}, SourceViewReceipt{}
	}
	copySelected := func(r SelectedReceipt) SelectedReceipt {
		r.Selected = append([]SelectedPathFact{}, r.Selected...)
		r.Stages = append([]IndexStageFact{}, r.Stages...)
		return r
	}
	copySource := func(r SourceViewReceipt) SourceViewReceipt {
		r.Facts = append([]SourceFact{}, r.Facts...)
		r.CandidatePaths = append([]string{}, r.CandidatePaths...)
		return r
	}
	return copySelected(p.a), copySelected(p.b), copySource(p.x), copySource(p.y)
}
func codecProblem() error {
	return unsupported("source_receipt_codec_invalid", "the local source receipt encoding is inconsistent")
}

// Explicit LOCAL v1 sets, including order, prevent silent widening when Go types
// change. These names are not a public report schema. Every field is required.
func receiptMembers(t reflect.Type) []string {
	switch t {
	case reflect.TypeOf(selectedWireEnvelope{}), reflect.TypeOf(sourceWireEnvelope{}):
		return []string{"kind", "receipt"}
	case reflect.TypeOf(SelectedReceipt{}):
		return strings.Fields("Version Pass RootIdentity Selected Stages PresentCount IndexedAbsentCount UnindexedAbsentCount SelectedInventoryDigest StageInventoryDigest TreeDigest BeginOffsetNS EndOffsetNS EffectiveLimits")
	case reflect.TypeOf(SelectedPathFact{}):
		return strings.Fields("Path State Mode ObjectDigest Size Indexed PathIdentity AbsenceParentIdentity")
	case reflect.TypeOf(IndexStageFact{}):
		return strings.Fields("Path Mode ObjectID Stage")
	case reflect.TypeOf(Limits{}):
		return strings.Fields("MaxFileBytes MaxTotalBytes MaxFiles")
	case reflect.TypeOf(SourceViewReceipt{}):
		return strings.Fields("Version Authority State ChainTiming Pass InvocationIdentity ExecutableDigest ExecutableNode CandidatePaths ConfigObject StateObject SourceDigest SelectedInventoryDigest StageInventoryDigest TreeDigest Facts SourceQueryAttempts SourceStdoutBytes SourceStderrBytes TotalDispatchAttempts PersistenceGuardAttempts ExecutableReads ExecutableReadBytes GuardStdoutBytes GuardStderrBytes MetadataReads MetadataBytes BeginOffsetNS EndOffsetNS")
	case reflect.TypeOf(SourceFact{}):
		return strings.Fields("Role Path State ObjectDigest NodeIdentity AbsenceIdentity Bytes")
	case reflect.TypeOf(sourceStateObject{}):
		return strings.Fields("Head Branch Facts")
	}
	return nil
}
func receiptFieldTypes(t reflect.Type) []string {
	switch t {
	case reflect.TypeOf(selectedWireEnvelope{}):
		return strings.Fields("string trees.SelectedReceipt")
	case reflect.TypeOf(sourceWireEnvelope{}):
		return strings.Fields("string trees.SourceViewReceipt")
	case reflect.TypeOf(SelectedReceipt{}):
		return strings.Fields("string uint8 string []trees.SelectedPathFact []trees.IndexStageFact uint32 uint32 uint32 string string string int64 int64 trees.Limits")
	case reflect.TypeOf(SelectedPathFact{}):
		return strings.Fields("string trees.SelectedPathState string string int64 bool string string")
	case reflect.TypeOf(IndexStageFact{}):
		return strings.Fields("string string string uint8")
	case reflect.TypeOf(Limits{}):
		return strings.Fields("int64 int64 int")
	case reflect.TypeOf(SourceViewReceipt{}):
		return strings.Fields("string string string string uint8 string string string []string string string string string string string []trees.SourceFact int int64 int64 int int int int64 int64 int64 int int64 int64 int64")
	case reflect.TypeOf(SourceFact{}):
		return strings.Fields("string string string string string string int64")
	case reflect.TypeOf(sourceStateObject{}):
		return strings.Fields("string string []trees.SourceFact")
	}
	return nil
}
func shapeMatches(t reflect.Type, names []string) bool {
	types := receiptFieldTypes(t)
	if t.Kind() != reflect.Struct || len(names) != t.NumField() || len(types) != len(names) {
		return false
	}
	for i, name := range names {
		f := t.Field(i)
		actual := f.Name
		if tag := f.Tag.Get("json"); tag != "" {
			actual = tag
		}
		if f.PkgPath != "" || f.Anonymous || actual != name || f.Type.String() != types[i] {
			return false
		}
	}
	return true
}
func receiptShapesMatch() bool {
	for _, v := range []any{selectedWireEnvelope{}, sourceWireEnvelope{}, SelectedReceipt{}, SelectedPathFact{}, IndexStageFact{}, Limits{}, SourceViewReceipt{}, SourceFact{}, sourceStateObject{}} {
		t := reflect.TypeOf(v)
		if !shapeMatches(t, receiptMembers(t)) {
			return false
		}
	}
	return true
}
func closedReceiptValue(v any, t reflect.Type) bool {
	if v == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		names := receiptMembers(t)
		m, ok := v.(map[string]any)
		if !ok || !shapeMatches(t, names) || len(m) != len(names) {
			return false
		}
		for i, name := range names {
			child, ok := m[name]
			if !ok || !closedReceiptValue(child, t.Field(i).Type) {
				return false
			}
		}
		return true
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, child := range a {
			if !closedReceiptValue(child, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.String:
		_, ok := v.(string)
		return ok
	case reflect.Bool:
		_, ok := v.(bool)
		return ok
	case reflect.Int, reflect.Int64:
		n, ok := v.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			return false
		}
		_, e := strconv.ParseInt(string(n), 10, t.Bits())
		return e == nil
	case reflect.Uint8, reflect.Uint32:
		n, ok := v.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			return false
		}
		_, e := strconv.ParseUint(string(n), 10, t.Bits())
		return e == nil
	}
	return false
}

// One bounded token core serves wire and original StateObject shapes. The existing
// JCS canonicalizer supplies canonical encoding; no general source/file reader exists.
func receiptJSONValue(d *json.Decoder, depth int, nodes *int) (any, error) {
	*nodes++
	if depth > 16 || *nodes > receiptJSONNodes {
		return nil, codecProblem()
	}
	tok, e := d.Token()
	if e != nil {
		return nil, codecProblem()
	}
	switch tok {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return nil, codecProblem()
			}
			k, ok := key.(string)
			if !ok {
				return nil, codecProblem()
			}
			if _, ok := m[k]; ok {
				return nil, codecProblem()
			}
			v, e := receiptJSONValue(d, depth+1, nodes)
			if e != nil {
				return nil, e
			}
			m[k] = v
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return nil, codecProblem()
		}
		return m, nil
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, e := receiptJSONValue(d, depth+1, nodes)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return nil, codecProblem()
		}
		return a, nil
	default:
		if _, ok := tok.(json.Delim); ok {
			return nil, codecProblem()
		}
		return tok, nil
	}
}
func decodeReceiptJSON(raw []byte, target any, canonical bool) error {
	if len(raw) == 0 || len(raw) > receiptWireMax || !utf8.Valid(raw) || !receiptShapesMatch() {
		return codecProblem()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	doc, e := receiptJSONValue(d, 0, &nodes)
	if e != nil {
		return codecProblem()
	}
	if _, e = d.Token(); e != io.EOF {
		return codecProblem()
	}
	t := reflect.TypeOf(target)
	if t.Kind() != reflect.Pointer || !closedReceiptValue(doc, t.Elem()) {
		return codecProblem()
	}
	if canonical {
		b, e := hint.Canonical(doc)
		if e != nil || !bytes.Equal(b, raw) {
			return codecProblem()
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(target) != nil || dec.Decode(new(any)) != io.EOF {
		return codecProblem()
	}
	return nil
}
