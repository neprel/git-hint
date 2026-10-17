package sessions

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

// ParseCodex normalizes one Codex CLI rollout JSONL file (Codex 0.161).
func ParseCodex(src []byte) (Result, error) {
	if len(src) > MaxSourceBytes {
		return Result{}, ErrTooLarge
	}
	lines, malformed, truncated := readJSONL(src)
	var meta map[string]any
	for _, l := range lines {
		if str(l.rec["type"]) == "session_meta" {
			meta, _ = l.rec["payload"].(map[string]any)
			break
		}
	}
	if meta == nil || str(meta["id"]) == "" {
		return Result{}, fmt.Errorf("%w: no Codex session_meta record", ErrFormat)
	}
	version := str(meta["cli_version"])
	if !strings.HasPrefix(version, "0.161.") {
		return Result{}, fmt.Errorf("%w: Codex %q (tested: 0.161)", ErrUnsupported, version)
	}
	b := newBuilder("codex", version, src)
	s := &b.res.Session
	s.Session, s.Parent, s.Directory = str(meta["id"]), str(meta["parent_thread_id"]), str(meta["cwd"])
	s.Malformed, s.Truncated = malformed, truncated
	s.Missing = []string{"task_id", "snapshots"}
	calls := map[string]int{}
	for _, l := range lines {
		p, _ := l.rec["payload"].(map[string]any)
		t := parseTime(str(l.rec["timestamp"]))
		rec := fmt.Sprintf("line:%d", l.n)
		if id := str(p["id"]); id != "" {
			rec = id
		}
		switch str(l.rec["type"]) {
		case "compacted":
			if err := b.add(Item{Record: rec, Line: l.n, Kind: KindCompaction, Time: t}); err != nil {
				return Result{}, err
			}
		case "token_usage_record":
			if u, ok := p["usage"].(map[string]any); ok {
				usage := &Usage{Input: i64(u["input_tokens"]), Output: i64(u["output_tokens"]), Cache: i64(u["cached_input_tokens"])}
				if err := b.add(Item{Record: rec, Line: l.n, Kind: KindAssistant, Time: t, Usage: usage}); err != nil {
					return Result{}, err
				}
			}
		case "response_item":
			switch str(p["type"]) {
			case "message":
				var texts []string
				content, _ := p["content"].([]any)
				for _, c := range content {
					cm, _ := c.(map[string]any)
					texts = append(texts, str(cm["text"]))
				}
				txt := strings.Join(texts, "\n")
				kind := KindSystem
				switch str(p["role"]) {
				case "user":
					if s.Parent == "" && !injected(txt) {
						kind = KindRequest
					}
				case "assistant":
					kind = KindAssistant
				}
				d, big := b.text(txt)
				if err := b.add(Item{Record: rec, Line: l.n, Kind: kind, Time: t, Text: d, Oversize: big}); err != nil {
					return Result{}, err
				}
			case "function_call", "custom_tool_call":
				name := str(p["name"])
				it := Item{Record: rec, Line: l.n, Kind: KindTool, Time: t, ToolName: name, Status: "unknown", Tool: ToolOther}
				input := str(p["input"])
				if a := str(p["arguments"]); a != "" {
					var args map[string]any
					if json.Unmarshal([]byte(a), &args) == nil {
						switch c := args["command"].(type) {
						case string:
							input = c
						case []any:
							parts := make([]string, 0, len(c))
							for _, x := range c {
								parts = append(parts, str(x))
							}
							input = strings.Join(parts, " ")
						}
						if input == "" {
							input = str(args["input"])
						}
					}
				}
				switch name {
				case "exec", "exec_command", "shell", "local_shell":
					it.Tool = ToolShell
					it.Command, it.Oversize = b.text(input)
				case "apply_patch":
					it.Tool = ToolEdit
					for _, m := range patchFile.FindAllStringSubmatch(input, -1) {
						it.Paths = append(it.Paths, strings.TrimSpace(m[1]))
					}
					it.Paths = uniq(it.Paths)
				case "spawn_agent":
					it.Tool = ToolChild
				}
				if err := b.add(it); err != nil {
					return Result{}, err
				}
				calls[str(p["call_id"])] = len(s.Items) - 1
			case "function_call_output", "custom_tool_call_output":
				if i, ok := calls[str(p["call_id"])]; ok {
					s.Items[i].Status = "completed"
				}
			}
		}
	}
	return b.res, nil
}
