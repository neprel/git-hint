package sessions

import (
	"bytes"
	"encoding/json"
)

type line struct {
	n   int
	rec map[string]any
}

// readJSONL splits src into JSON records with their line numbers. A record that does not
// parse counts as malformed; a last line without a newline that does not parse marks the
// source as truncated (a session that a crash or rotation cut).
func readJSONL(src []byte) (lines []line, malformed int, truncated bool) {
	parts := bytes.Split(src, []byte("\n"))
	for i, p := range parts {
		p = bytes.TrimSpace(p)
		if len(p) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			if i == len(parts)-1 {
				truncated = true
			} else {
				malformed++
			}
			continue
		}
		lines = append(lines, line{n: i + 1, rec: m})
	}
	return lines, malformed, truncated
}

// injected reports text that an agent or tool inserted into a user turn (tags such as
// <command-name> or <environment_context>, interruption notices), not text a human typed.
func injected(s string) bool {
	t := bytes.TrimSpace([]byte(s))
	return len(t) == 0 || t[0] == '<' || bytes.HasPrefix(t, []byte("[Request interrupted")) || bytes.HasPrefix(t, []byte("# AGENTS.md"))
}
