package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenCodeOptions describe an OpenCode export that the export itself does not state.
type OpenCodeOptions struct {
	Version string // OpenCode version that wrote the export (`opencode --version`)
	Parent  string // parent session ID for a child session, from `opencode session list`
}

var openCodeTools = map[string]string{
	"read": ToolRead, "glob": ToolSearch, "grep": ToolSearch, "list": ToolSearch, "codesearch": ToolSearch,
	"edit": ToolEdit, "write": ToolEdit, "patch": ToolEdit, "multiedit": ToolEdit, "apply_patch": ToolEdit,
	"bash": ToolShell, "task": ToolChild,
	// OpenCode 2.0.22 (recorded export): shell commands and child agents
	"shell": ToolShell, "subagent": ToolChild,
}

// ParseOpenCode normalizes one `opencode session export` document (OpenCode 2.0).
func ParseOpenCode(src []byte, opt OpenCodeOptions) (Result, error) {
	if len(src) > MaxSourceBytes {
		return Result{}, ErrTooLarge
	}
	if !strings.HasPrefix(opt.Version, "2.0.") {
		return Result{}, fmt.Errorf("%w: OpenCode %q (tested: 2.0)", ErrUnsupported, opt.Version)
	}
	var doc struct {
		Info     map[string]any   `json:"info"`
		Messages []map[string]any `json:"messages"`
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	if err := dec.Decode(&doc); err != nil {
		return Result{}, fmt.Errorf("%w: OpenCode export does not parse (truncated or malformed)", ErrFormat)
	}
	if doc.Info == nil || str(doc.Info["id"]) == "" || doc.Messages == nil {
		return Result{}, fmt.Errorf("%w: OpenCode export without info.id or messages", ErrFormat)
	}
	b := newBuilder("opencode", opt.Version, src)
	s := &b.res.Session
	s.Session, s.Parent, s.Project = str(doc.Info["id"]), opt.Parent, str(doc.Info["projectID"])
	if loc, ok := doc.Info["location"].(map[string]any); ok {
		s.Directory = str(loc["directory"])
	}
	s.Missing = []string{"task_id", "compaction_markers"}
	if opt.Parent == "" {
		s.Missing = append(s.Missing, "parent_link")
	}
	for i, m := range doc.Messages {
		id := str(m["id"])
		if id == "" {
			s.Malformed++
			continue
		}
		t := timeMS(m["time"], "created")
		switch str(m["type"]) {
		case "user":
			kind := KindRequest
			if opt.Parent != "" { // a child session's prompt comes from the parent agent
				kind = KindSystem
			}
			d, big := b.text(str(m["text"]))
			if err := b.add(Item{Record: id, Kind: kind, Time: t, Text: d, Oversize: big}); err != nil {
				return Result{}, err
			}
		case "system":
			d, big := b.text(str(m["text"]))
			if err := b.add(Item{Record: id, Kind: KindSystem, Time: t, Text: d, Oversize: big}); err != nil {
				return Result{}, err
			}
		case "assistant":
			snap, _ := m["snapshot"].(map[string]any)
			usage := tokens(m["tokens"])
			var texts []string
			content, _ := m["content"].([]any)
			for j, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					s.Malformed++
					continue
				}
				switch str(cm["type"]) {
				case "text":
					texts = append(texts, str(cm["text"]))
				case "tool":
					it, err := openCodeTool(b, cm, fmt.Sprintf("%s/%d", id, j))
					if err != nil {
						return Result{}, err
					}
					it.Time = timeMS(cm["time"], "created")
					if err := b.add(it); err != nil {
						return Result{}, err
					}
				}
			}
			d, big := b.text(strings.Join(texts, "\n"))
			it := Item{Record: id, Kind: KindAssistant, Time: t, Text: d, Oversize: big, Usage: usage}
			if snap != nil {
				it.SnapStart, it.SnapEnd = str(snap["start"]), str(snap["end"])
			}
			if err := b.add(it); err != nil {
				return Result{}, err
			}
		default:
			s.Malformed++
		}
		_ = i
	}
	return b.res, nil
}

func openCodeTool(b *builder, c map[string]any, fallbackID string) (Item, error) {
	name := str(c["name"])
	id := str(c["id"])
	if id == "" {
		id = fallbackID
	}
	class, ok := openCodeTools[name]
	if !ok {
		class = ToolOther
	}
	it := Item{Record: id, Kind: KindTool, Tool: class, ToolName: name, Status: "unknown"}
	st, _ := c["state"].(map[string]any)
	if st == nil {
		return it, nil
	}
	if v := str(st["status"]); v != "" {
		it.Status = v
	}
	if in, ok := st["input"].(map[string]any); ok {
		var paths []string
		for _, k := range []string{"filePath", "file_path", "path"} {
			paths = append(paths, str(in[k]))
		}
		if fs, ok := in["files"].([]any); ok {
			for _, f := range fs {
				paths = append(paths, str(f))
			}
		}
		it.Paths = uniq(paths)
		if cmd := str(in["command"]); cmd != "" {
			it.Command, it.Oversize = b.text(cmd)
		}
	}
	if md, ok := st["metadata"].(map[string]any); ok {
		it.Child = str(md["sessionId"])
		// OpenCode 2 marks a finished command "completed" whatever its exit code and puts
		// the code into the metadata: a non-zero exit is a failed command
		if e := i64(md["exit"]); e != nil && *e != 0 && it.Status == "completed" {
			it.Status = "error"
		}
	}
	return it, nil
}

func timeMS(v any, key string) int64 {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	if p := i64(m[key]); p != nil {
		return *p
	}
	return 0
}

func tokens(v any) *Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	u := &Usage{Input: i64(m["input"]), Output: i64(m["output"])}
	if c, ok := m["cache"].(map[string]any); ok {
		if r := i64(c["read"]); r != nil {
			u.Cache = r
		}
	}
	return u
}
