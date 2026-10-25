package runjournal

import "github.com/neprel/git-hint/internal/hint"

func ScopeConfiguration(key []byte, m Manifest) map[string]any {
	ref := func(kind string, parts ...string) string { return hint.EvalRef(key, kind, parts...) }
	limit := func(v int) map[string]any {
		if v >= 1 {
			return map[string]any{"state": "known", "value": v}
		}
		return map[string]any{"state": "unknown"}
	}
	harness := m.Harness
	if harness == "" {
		harness = "harbor"
	}
	return map[string]any{
		"agent":          map[string]any{"name": m.Agent, "version": knownVersion(m.AgentVersion)},
		"model":          map[string]any{"state": "known", "ref": ref("model", m.ModelLabel), "provenance": "launch_intent"},
		"harness":        map[string]any{"kind": harness, "version": knownVersion(m.RunnerVersion)},
		"context_policy": map[string]any{"state": "disabled"}, // HINT delivers no managed context in eval trials yet
		"config_refs":    map[string]any{"state": "known", "refs": []any{ref("observed-config", "setting_override", m.LaunchDigest)}},
		"budget":         map[string]any{"max_requests": limit(m.MaxRequests), "max_seconds": limit(m.MaxSeconds)},
	}
}

func knownVersion(v string) map[string]any {
	if v == "" {
		return map[string]any{"state": "unknown"}
	}
	return map[string]any{"state": "known", "value": v}
}
