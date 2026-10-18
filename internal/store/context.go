package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/hint"
)

// ContextImpl names the retrieval implementation of hint_context; it is part of the identity
// of every effective context policy, so a changed implementation is a changed policy.
const ContextImpl = "hint_context/1"

// Context limit ranges of hint_context (also the ranges of its call arguments).
var ContextRanges = map[string][3]int{ // key: default, min, max
	"depth": {1, 1, 2}, "max_entities": {20, 1, 100}, "max_bytes": {16 << 10, 4096, 64 << 10}, "max_ms": {1000, 100, 5000},
}

// ContextLimits are the limits of one hint_context answer.
type ContextLimits struct {
	Depth       int `json:"depth"`
	MaxEntities int `json:"max_entities"`
	MaxBytes    int `json:"max_bytes"`
	MaxMillis   int `json:"max_ms"`
}

// ContextVersion is one immutable version of the local context policy: limits, or nil when
// the change turned the policy off.
type ContextVersion struct {
	Version      int            `json:"version"`
	Limits       *ContextLimits `json:"limits"`
	Change       string         `json:"change"` // set, off, restore
	RestoredFrom int            `json:"restored_from,omitempty"`
	ChangedAt    string         `json:"changed_at"`
}

// ContextPolicy is the local, HINT-managed default of hint_context, with every earlier
// version kept for inspection and restore. Only the user changes it (git hint init
// --context-limits); no MCP tool can.
type ContextPolicy struct {
	Current  int              `json:"current"`
	Versions []ContextVersion `json:"versions"`
}

// Active returns the limits of the current version, or nil without an active policy.
func (p *ContextPolicy) Active() *ContextLimits {
	if p == nil || p.Current < 1 || p.Current > len(p.Versions) {
		return nil
	}
	return p.Versions[p.Current-1].Limits
}

// ContextPolicyRef is the project reference of an effective policy identity: the canonical
// effective limits (after defaults and call arguments) and the implementation version.
func ContextPolicyRef(key []byte, l ContextLimits) string {
	b, _ := json.Marshal(map[string]any{"depth": l.Depth, "impl": ContextImpl, "max_bytes": l.MaxBytes, "max_entities": l.MaxEntities, "max_ms": l.MaxMillis})
	return hint.DeriveID(key, "context-policy", string(b))
}

// ContextPolicyIdentity is the canonical identity of the current local policy for the
// configuration identity (C7B-E): state enabled with (id, version, digest of the canonical
// limits and implementation), or disabled without an active policy. The project reference is
// hint.EvalRef(key, "context-policy", id, version, digest), as in eval reports.
func (c *Config) ContextPolicyIdentity() (state, id string, version int, digest string) {
	l := c.Context.Active()
	if l == nil {
		return "disabled", "", 0, ""
	}
	b, _ := json.Marshal(map[string]any{"depth": l.Depth, "impl": ContextImpl, "max_bytes": l.MaxBytes, "max_entities": l.MaxEntities, "max_ms": l.MaxMillis})
	var v any
	_ = json.Unmarshal(b, &v)
	d, _ := hint.Digest(v)
	return "enabled", "hint-context", c.Context.Current, d
}

// applyContextLimits changes the policy: "off", "restore=N", or "key=value,..." (keys depth,
// items, bytes, ms; unnamed keys take the defaults). Each change is a new version.
func (c *Config) applyContextLimits(spec string, now time.Time) error {
	if c.Context == nil {
		c.Context = &ContextPolicy{}
	}
	p := c.Context
	v := ContextVersion{Version: len(p.Versions) + 1, ChangedAt: now.UTC().Format(time.RFC3339)}
	switch {
	case spec == "off":
		v.Change = "off"
	case strings.HasPrefix(spec, "restore="):
		n, err := strconv.Atoi(strings.TrimPrefix(spec, "restore="))
		if err != nil || n < 1 || n > len(p.Versions) {
			return fmt.Errorf("context limits: no version %q to restore", strings.TrimPrefix(spec, "restore="))
		}
		v.Change, v.RestoredFrom, v.Limits = "restore", n, p.Versions[n-1].Limits
	default:
		l := ContextLimits{ContextRanges["depth"][0], ContextRanges["max_entities"][0], ContextRanges["max_bytes"][0], ContextRanges["max_ms"][0]}
		dst := map[string]*int{"depth": &l.Depth, "items": &l.MaxEntities, "bytes": &l.MaxBytes, "ms": &l.MaxMillis}
		rng := map[string]string{"depth": "depth", "items": "max_entities", "bytes": "max_bytes", "ms": "max_ms"}
		for _, kv := range strings.Split(spec, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
			n, err := strconv.Atoi(val)
			r, known := ContextRanges[rng[k]]
			if !ok || err != nil || !known || n < r[1] || n > r[2] {
				return fmt.Errorf("context limits %q: use depth=1-2, items=1-100, bytes=4096-65536, ms=100-5000, off or restore=N", kv)
			}
			*dst[k] = n
		}
		v.Change, v.Limits = "set", &l
	}
	p.Versions = append(p.Versions, v)
	p.Current = v.Version
	return nil
}
