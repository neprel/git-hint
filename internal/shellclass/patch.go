package shellclass

import "strings"

// codexPatchTargets reads the Codex apply_patch envelope:
//
//	*** Begin Patch
//	*** Update File: path
//	*** Add File: path
//	*** Delete File: path
//	*** Move to: path
//	*** End Patch
//
// It returns written paths, deleted paths and ok=false when the text has
// no envelope.
func codexPatchTargets(body string) (writes, deletes []string, ok bool) {
	if !strings.Contains(body, "*** Begin Patch") {
		return nil, nil, false
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			writes = append(writes, strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: ")))
		case strings.HasPrefix(line, "*** Add File: "):
			writes = append(writes, strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: ")))
		case strings.HasPrefix(line, "*** Move to: "):
			writes = append(writes, strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: ")))
		case strings.HasPrefix(line, "*** Delete File: "):
			deletes = append(deletes, strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: ")))
		}
	}
	return writes, deletes, true
}

// unifiedDiffTargets reads "+++ " headers (and "--- " for deleted files)
// from a unified diff and strips strip leading path components.
func unifiedDiffTargets(body string, strip int) (writes, deletes []string) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "+++ ") {
			continue
		}
		p := headerPath(strings.TrimPrefix(line, "+++ "))
		if p == "/dev/null" && i > 0 && strings.HasPrefix(lines[i-1], "--- ") {
			if old := stripPath(headerPath(strings.TrimPrefix(lines[i-1], "--- ")), strip); old != "" {
				deletes = append(deletes, old)
			}
			continue
		}
		if t := stripPath(p, strip); t != "" {
			writes = append(writes, t)
		}
	}
	return writes, deletes
}

func headerPath(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func stripPath(p string, n int) string {
	if p == "/dev/null" || p == "" {
		return ""
	}
	for ; n > 0; n-- {
		i := strings.IndexByte(p, '/')
		if i < 0 {
			return p
		}
		p = p[i+1:]
	}
	return p
}
