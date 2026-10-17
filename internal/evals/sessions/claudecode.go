package sessions

import (
	"fmt"
	"strings"
	"time"
)

var claudeTools = map[string]string{
	"Read": ToolRead, "Grep": ToolSearch, "Glob": ToolSearch, "LS": ToolSearch,
	"Edit": ToolEdit, "Write": ToolEdit, "MultiEdit": ToolEdit, "NotebookEdit": ToolEdit,
	"Bash": ToolShell, "Task": ToolChild, "Agent": ToolChild,
}

// ParseClaudeCode normalizes one Claude Code session JSONL file (Claude Code 2.1).
func ParseClaudeCode(src []byte) (Result, error) {
	if len(src) > MaxSourceBytes {
		return Result{}, ErrTooLarge
	}
	lines, malformed, truncated := readJSONL(src)
	version, session := "", ""
	for _, l := range lines {
		if v := str(l.rec["version"]); v != "" && version == "" {
			version = v
		}
		if v := str(l.rec["sessionId"]); v != "" && session == "" {
			session = v
		}
	}
	if session == "" {
		return Result{}, fmt.Errorf("%w: no Claude Code session records", ErrFormat)
	}
	if !strings.HasPrefix(version, "2.1.") {
		return Result{}, fmt.Errorf("%w: Claude Code %q (tested: 2.1)", ErrUnsupported, version)
	}
	b := newBuilder("claude-code", version, src)
	s := &b.res.Session
	s.Session, s.Malformed, s.Truncated = session, malformed, truncated
	s.Missing = []string{"task_id", "parent_link", "snapshots"}
	tools := map[string]int{} // tool_use id -> item index
	usageSeen := map[string]bool{}
	for _, l := range lines {
		r := l.rec
		id := str(r["uuid"])
		if id == "" {
			id = fmt.Sprintf("line:%d", l.n)
		}
		t := parseTime(str(r["timestamp"]))
		if s.Directory == "" {
			s.Directory = str(r["cwd"])
		}
		side, _ := r["isSidechain"].(bool)
		meta, _ := r["isMeta"].(bool)
		switch str(r["type"]) {
		case "user":
			msg, _ := r["message"].(map[string]any)
			switch c := msg["content"].(type) {
			case string:
				kind := KindRequest
				if meta || side || injected(c) {
					kind = KindSystem
				}
				d, big := b.text(c)
				if err := b.add(Item{Record: id, Line: l.n, Kind: kind, Time: t, Text: d, Oversize: big}); err != nil {
					return Result{}, err
				}
			case []any:
				var texts []string
				for _, blk := range c {
					bm, _ := blk.(map[string]any)
					switch str(bm["type"]) {
					case "tool_result":
						if i, ok := tools[str(bm["tool_use_id"])]; ok {
							s.Items[i].Status = "completed"
							if e, _ := bm["is_error"].(bool); e {
								s.Items[i].Status = "error"
							}
						}
					case "text":
						texts = append(texts, str(bm["text"]))
					}
				}
				if len(texts) > 0 {
					txt := strings.Join(texts, "\n")
					kind := KindRequest
					if meta || side || injected(txt) {
						kind = KindSystem
					}
					d, big := b.text(txt)
					if err := b.add(Item{Record: id, Line: l.n, Kind: kind, Time: t, Text: d, Oversize: big}); err != nil {
						return Result{}, err
					}
				}
			}
		case "assistant":
			msg, _ := r["message"].(map[string]any)
			var usage *Usage
			if mid := str(msg["id"]); mid != "" && !usageSeen[mid] { // one response spans several lines
				if u, ok := msg["usage"].(map[string]any); ok {
					usageSeen[mid] = true
					usage = &Usage{Input: i64(u["input_tokens"]), Output: i64(u["output_tokens"]), Cache: i64(u["cache_read_input_tokens"])}
				}
			}
			blocks, _ := msg["content"].([]any)
			var texts []string
			for _, blk := range blocks {
				bm, _ := blk.(map[string]any)
				switch str(bm["type"]) {
				case "text":
					texts = append(texts, str(bm["text"]))
				case "tool_use":
					name := str(bm["name"])
					class, ok := claudeTools[name]
					if !ok {
						class = ToolOther
					}
					it := Item{Record: str(bm["id"]), Line: l.n, Kind: KindTool, Time: t, Tool: class, ToolName: name, Status: "unknown"}
					if it.Record == "" {
						it.Record = id + "/tool"
					}
					if in, ok := bm["input"].(map[string]any); ok {
						it.Paths = uniq([]string{str(in["file_path"]), str(in["path"]), str(in["notebook_path"])})
						if cmd := str(in["command"]); cmd != "" {
							it.Command, it.Oversize = b.text(cmd)
						}
					}
					if err := b.add(it); err != nil {
						return Result{}, err
					}
					tools[str(bm["id"])] = len(s.Items) - 1
				}
			}
			if len(texts) > 0 || usage != nil {
				d, big := b.text(strings.Join(texts, "\n"))
				if err := b.add(Item{Record: id, Line: l.n, Kind: KindAssistant, Time: t, Text: d, Oversize: big, Usage: usage}); err != nil {
					return Result{}, err
				}
			}
		case "system":
			if str(r["subtype"]) == "compact_boundary" {
				if err := b.add(Item{Record: id, Line: l.n, Kind: KindCompaction, Time: t}); err != nil {
					return Result{}, err
				}
			}
		case "summary":
			if err := b.add(Item{Record: id, Line: l.n, Kind: KindCompaction, Time: t}); err != nil {
				return Result{}, err
			}
		}
	}
	return b.res, nil
}

func parseTime(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
