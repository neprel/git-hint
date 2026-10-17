// Package sessions imports coding-agent session records into a normalized form for eval
// discovery. It reads files only; it never starts an agent and never runs a command found
// in a session.
//
// Supported inputs (exact tested versions; other versions are refused, not guessed):
//   - OpenCode 2.0: `opencode session export <id>` JSON (tested: 2.0.26).
//   - Claude Code 2.1: session JSONL (tested: 2.1.302).
//   - Codex CLI 0.161: rollout JSONL (tested: 0.161.0).
//
// Session text is sensitive. The normalized session holds digests of text; the texts come
// back separately so the caller can store them in the local library. Diagnostics carry
// codes and record positions only, never text.
package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// ParserVersion is the version of the normalization rules.
const ParserVersion = "0.1.0"

// Limits applied before any parsing.
const (
	MaxSourceBytes = 64 << 20
	MaxFieldBytes  = 1 << 20
	MaxItems       = 200000
)

// Kinds of normalized items.
const (
	KindRequest    = "request"    // text a human user typed
	KindAssistant  = "assistant"  // assistant text
	KindTool       = "tool"       // one tool call with its outcome
	KindCompaction = "compaction" // the agent compacted or replaced its history
	KindSystem     = "system"     // injected or system text (never a request)
)

// Item is one normalized record of a session.
type Item struct {
	Seq       int      `json:"seq"`
	Record    string   `json:"record"` // stable record identity in the source
	Line      int      `json:"line,omitempty"`
	Kind      string   `json:"kind"`
	Time      int64    `json:"time_ms,omitempty"` // 0 = unknown
	Text      string   `json:"text,omitempty"`    // digest of the text object
	Tool      string   `json:"tool,omitempty"`    // normalized tool class
	ToolName  string   `json:"tool_name,omitempty"`
	Status    string   `json:"status,omitempty"`  // completed, error, unknown
	Paths     []string `json:"paths,omitempty"`   // file paths named by the tool input (sensitive)
	Command   string   `json:"command,omitempty"` // digest of the command text (sensitive)
	Child     string   `json:"child,omitempty"`   // child session ID started by this call
	SnapStart string   `json:"snapshot_start,omitempty"`
	SnapEnd   string   `json:"snapshot_end,omitempty"`
	Usage     *Usage   `json:"usage,omitempty"`
	Oversize  bool     `json:"oversize,omitempty"`
}

// Usage is the token use of one response; nil fields are unknown.
type Usage struct {
	Input  *int64 `json:"input"`
	Output *int64 `json:"output"`
	Cache  *int64 `json:"cache"`
}

// Tool classes.
const (
	ToolRead   = "read"
	ToolSearch = "search"
	ToolEdit   = "edit"
	ToolShell  = "shell"
	ToolChild  = "child"
	ToolOther  = "other"
)

// Session is one normalized session.
type Session struct {
	Agent         string   `json:"agent"`
	AgentVersion  string   `json:"agent_version"`
	ParserVersion string   `json:"parser_version"`
	Source        string   `json:"source"` // digest of the source bytes
	Session       string   `json:"session"`
	Parent        string   `json:"parent,omitempty"`
	Directory     string   `json:"directory,omitempty"` // sensitive
	Items         []Item   `json:"items"`
	Truncated     bool     `json:"truncated,omitempty"` // the source ended inside a record
	Malformed     int      `json:"malformed,omitempty"` // records that could not be read
	Missing       []string `json:"missing"`             // fields this source cannot provide
}

// Result is a parsed session plus the texts it references (digest -> text).
type Result struct {
	Session Session
	Texts   map[string][]byte
}

// Errors of the importers. Their messages never hold session text.
var (
	ErrTooLarge    = errors.New("session source exceeds the size limit")
	ErrUnsupported = errors.New("unsupported session format version")
	ErrFormat      = errors.New("not a session record of the expected format")
)

// Digest returns the content digest of b.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

type builder struct {
	res   Result
	items int
}

func newBuilder(agent, version string, src []byte) *builder {
	return &builder{res: Result{Session: Session{Agent: agent, AgentVersion: version, ParserVersion: ParserVersion,
		Source: Digest(src), Items: []Item{}, Missing: []string{}}, Texts: map[string][]byte{}}}
}

// text stores a text and returns its digest; oversize text is not stored.
func (b *builder) text(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if len(s) > MaxFieldBytes {
		return "", true
	}
	d := Digest([]byte(s))
	b.res.Texts[d] = []byte(s)
	return d, false
}

func (b *builder) add(it Item) error {
	if b.items >= MaxItems {
		return fmt.Errorf("%w: more than %d records", ErrTooLarge, MaxItems)
	}
	b.items++
	it.Seq = len(b.res.Session.Items) + 1
	sort.Strings(it.Paths)
	b.res.Session.Items = append(b.res.Session.Items, it)
	return nil
}

func i64(v any) *int64 {
	f, ok := v.(float64)
	if !ok || f < 0 {
		return nil
	}
	n := int64(f)
	return &n
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
