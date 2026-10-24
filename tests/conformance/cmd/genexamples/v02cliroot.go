package main

import "github.com/neprel/git-hint/tests/conformance/hintconf"

// cliRootSource is a root CLI adapter source (opencode-cli-root 0.1.0): exactly the adapter's
// scope, with the declared task, family and run correlations.
func cliRootSource() M {
	return M{"source_id": id("source", "opencode-cli-root"), "agent": "opencode", "agent_version": "2.0.26",
		"adapter": M{"name": "opencode-cli-root", "version": "0.1.0"},
		"capabilities": A{"time.start", "time.end", "targets", "outcome.status", "outcome.exit_code", "size", "content_delivery", "command_class",
			"correlation.session_id", "correlation.tool_call_id", "correlation.task_id", "correlation.task_family_id", "correlation.run_ref"},
		"unavailable": A{"usage", "request_kind", "collection", "correlation.turn_id", "correlation.trace_id", "correlation.span_id",
			"coordination", "context"}}
}

// cliRootDoc is the context example plus a root CLI adapter source and its log-derived copies of
// the example's plugin READ and EXECUTE observations (no turn, no context measurement: the scope
// cannot observe them).
func cliRootDoc(act M) M {
	d := contextDoc(act)
	d["document_id"] = id("document", "activity", "cli-root")
	src := cliRootSource()
	d["sources"] = append(d["sources"].([]any), src)
	obs := obj(d, "payload")["observations"].([]any)
	var add []any
	for _, o := range obs {
		m := o.(map[string]any)
		if m["source"] != "plugin" || (m["op"] != "READ" && m["op"] != "EXECUTE") {
			continue
		}
		r := plain(m)
		r["observation_id"] = id("cli-root", m["observation_id"].(string))
		r["source"], r["source_id"] = "log", src["source_id"]
		delete(r["correlation"].(map[string]any), "turn_id")
		delete(r, "context") // the scope cannot observe a context measurement
		add = append(add, r)
	}
	obj(d, "payload")["observations"] = append(obs, add...)
	return reseal(d)
}

// cliRootInvalid are the negative cases of the root CLI adapter source and its observations.
func cliRootInvalid() []invalidCase {
	const base = "activity-cli-root.json"
	src := func(d M) M {
		ss := d["sources"].([]any)
		return ss[len(ss)-1].(map[string]any)
	}
	last := func(d M) M {
		obs := obj(d, "payload")["observations"].([]any)
		return obs[len(obs)-1].(map[string]any)
	}
	semantic := func(name string, f func(d M)) invalidCase {
		return invalidCase{name, base, "semantic", hintconf.CodeSourceCLIRoot, f, nil}
	}
	drop := func(list []any, v string) []any {
		var out []any
		for _, x := range list {
			if x != v {
				out = append(out, x)
			}
		}
		return out
	}
	return []invalidCase{
		{"cli-root-unknown-adapter-name", base, "schema", "source", func(d M) { src(d)["adapter"].(map[string]any)["name"] = "opencode-cli" }, nil},
		semantic("cli-root-claims-usage", func(d M) { src(d)["capabilities"] = append(src(d)["capabilities"].([]any), "usage") }),
		semantic("cli-root-claims-collection", func(d M) { src(d)["capabilities"] = append(src(d)["capabilities"].([]any), "collection") }),
		semantic("cli-root-missing-capability", func(d M) { src(d)["capabilities"] = drop(src(d)["capabilities"].([]any), "size") }),
		semantic("cli-root-collection-not-unavailable", func(d M) { src(d)["unavailable"] = drop(src(d)["unavailable"].([]any), "collection") }),
		semantic("cli-root-usage-normalization", func(d M) { src(d)["usage_normalization"] = "openai-compat-usage-v1" }),
		semantic("cli-root-observation-not-log", func(d M) { last(d)["source"] = "plugin" }),
		semantic("cli-root-observation-turn", func(d M) { last(d)["correlation"].(map[string]any)["turn_id"] = id("turn", "t") }),
	}
}
