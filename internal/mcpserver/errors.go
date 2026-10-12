package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/store"
)

// ToolError is the only error form that a tool returns. Code is a stable token; Message
// is a fixed sentence without names, paths or source text.
type ToolError struct {
	Code    string
	Message string
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

var messages = map[string]string{
	"not_initialized":        "No HINT project here. The user runs `git hint init` in the repository root.",
	"not_excluded":           "The .hint/ store is not ignored by Git, or Git tracks a file in it. Nothing was written. The user runs `git hint doctor`.",
	"key_location":           "The project key is not in an allowed place. The user runs `git hint doctor`.",
	"busy":                   "Another HINT operation runs on this project. Try again when it ends.",
	"cancelled":              "The operation was cancelled. Local data is unchanged or complete.",
	"timeout":                "The operation took too long and was stopped. Local data is unchanged or complete.",
	"no_snapshot":            "A repository has no snapshot. Call hint_collect first.",
	"policy_invalid":         "The local export policy is not valid. The user checks the policy in .hint/state/project.json.",
	"document_invalid":       "A document failed validation and was not exported.",
	"server_unconfigured":    "No HINT server is configured. The user sets it with `git hint init --server <url>`.",
	"network_queued":         "The server could not be reached. The package is queued; the user can run `git hint recommend --retry`.",
	"server_rejected":        "The server rejected a document.",
	"receipt_invalid":        "The server answer did not confirm the sent documents. The export is not marked as done.",
	"recommendation_missing": "No stored recommendation matches. Call hint_recommend or hint_import_recommendation first.",
	"invalid_argument":       "An argument is not valid.",
	"path_not_allowed":       "The path is outside the project repositories, or the file exists and is not a HINT plan.",
	"too_large":              "The input is larger than the limit.",
	"internal":               "The operation failed. The user can run the matching `git hint` command for details.",
}

func toolErr(code string) *ToolError { return &ToolError{Code: code, Message: messages[code]} }

func toolErrf(code, detail string) *ToolError {
	return &ToolError{Code: code, Message: strings.TrimSpace(messages[code] + " " + detail)}
}

// classify maps an internal error to a redacted tool error. The detail never reaches the
// client; it goes to the stderr diagnostics only as its code.
func classify(err error) *ToolError {
	var te *ToolError
	var q *project.ErrQueued
	switch {
	case err == nil:
		return nil
	case errors.As(err, &te):
		return te
	case errors.Is(err, context.Canceled):
		return toolErr("cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return toolErr("timeout")
	case errors.Is(err, store.ErrBusy):
		return toolErr("busy")
	case errors.Is(err, store.ErrNotExcluded):
		return toolErr("not_excluded")
	case errors.Is(err, store.ErrKeyInWorkTree):
		return toolErr("key_location")
	case errors.As(err, &q), errors.Is(err, api.ErrNetwork):
		return toolErr("network_queued")
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no HINT project here"):
		return toolErr("not_initialized")
	case strings.Contains(msg, "has no snapshot"):
		return toolErr("no_snapshot")
	case strings.Contains(msg, "export policy"), strings.Contains(msg, "profile"):
		return toolErr("policy_invalid")
	case strings.Contains(msg, "(the document is not exported)"):
		// keep only the rule code, an enumeration value
		code := ""
		if i := strings.Index(msg, ": "); i > 0 {
			rest := msg[i+2:]
			code = strings.Fields(rest)[0]
		}
		return toolErrf("document_invalid", fmt.Sprintf("Rule: %s.", sanitizeToken(code)))
	case strings.Contains(msg, "server rejected"):
		return toolErrf("server_rejected", fmt.Sprintf("Code: %s.", sanitizeToken(msg[strings.LastIndex(msg, " ")+1:])))
	case strings.Contains(msg, "receipt"):
		return toolErr("receipt_invalid")
	case strings.Contains(msg, "no stored recommendation"):
		return toolErr("recommendation_missing")
	}
	return toolErr("internal")
}

// sanitizeToken keeps an enumeration-like token (letters, digits, _ and -) or drops it.
func sanitizeToken(s string) string {
	s = strings.Trim(s, ".,:;()")
	if len(s) == 0 || len(s) > 40 {
		return "unknown"
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "unknown"
		}
	}
	return s
}

// withTimeout bounds a tool context.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
