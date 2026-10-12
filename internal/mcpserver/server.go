// Package mcpserver is the MCP interface of git-hint (`git hint mcp`). It runs on stdio
// with the official MCP Go SDK and calls the same services as the CLI commands: it has no
// collector, exporter or validator of its own.
//
// Rules:
//   - stdout carries MCP JSON-RPC messages only; diagnostics go to stderr;
//   - every tool states its side effect (none, local write, network);
//   - the agent's request alone never sends data: hint_recommend needs the local rule
//     `mcp.upload: allow` in .hint/state/project.json and uses only the configured server;
//   - tool errors carry a code and a fixed message, never a name, a path or source text;
//   - local results may name local files and symbols; nothing leaves the machine except
//     the validated export package.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/api"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

// Options configure the server.
type Options struct {
	Dir    string           // project directory (the store is found from here upwards)
	Now    func() time.Time // clock (tests)
	Stderr io.Writer        // diagnostics
	// NewClient creates the HINT API client (tests replace it); default api.New.
	NewClient func(url string) (*api.Client, error)
	// Limits
	CollectTimeout   time.Duration
	NetworkTimeout   time.Duration
	MaxMarkdownBytes int
	MaxListItems     int
	// Tools limits the served tools to these names (empty: all tools).
	Tools []string
}

func (o *Options) defaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.NewClient == nil {
		o.NewClient = api.New
	}
	if o.CollectTimeout == 0 {
		o.CollectTimeout = 10 * time.Minute
	}
	if o.NetworkTimeout == 0 {
		o.NetworkTimeout = 5 * time.Minute
	}
	if o.MaxMarkdownBytes == 0 {
		o.MaxMarkdownBytes = 64 << 10
	}
	if o.MaxListItems == 0 {
		o.MaxListItems = 200
	}
}

// Instructions are sent to the client at initialization.
const Instructions = `HINT measures repository structure and agent activity locally and exchanges
content-free documents with a HINT server. Typical order: hint_status, hint_collect,
hint_export_preview, hint_recommend (needs the user's local upload rule), hint_plan.
Results of hint_recommend and hint_plan are recommendations, not accepted changes:
review the plan and run the checks before you change code. HINT never changes code.`

// New builds the MCP server with all tools.
func New(o Options) *mcp.Server {
	o.defaults()
	h := &handlers{o: o}
	s := mcp.NewServer(&mcp.Implementation{Name: "git-hint", Title: "HINT", Version: version.Version},
		&mcp.ServerOptions{Instructions: Instructions, InitializedHandler: h.initialized})
	h.register(s)
	if len(o.Tools) > 0 {
		var drop []string
		for _, n := range ToolNames {
			if !slices.Contains(o.Tools, n) {
				drop = append(drop, n)
			}
		}
		s.RemoveTools(drop...)
	}
	return s
}

// ToolNames lists every tool of the server.
var ToolNames = []string{"hint_status", "hint_collect", "hint_export_preview", "hint_recommend", "hint_plan",
	"hint_import_recommendation", "hint_changes", "hint_evidence", "hint_compare",
	"hint_find_definition", "hint_importers", "hint_file_outline"}

// Run serves MCP on the given streams until the client disconnects or ctx ends.
func Run(ctx context.Context, o Options, in io.ReadCloser, out io.WriteCloser) error {
	return New(o).Run(ctx, &mcp.IOTransport{Reader: in, Writer: out})
}

// initialized records the client and the protocol version in .hint/state/mcp.log (local
// diagnostics; it holds no project data). The negotiated version is the client's version
// when the SDK supports it, else the SDK's latest version.
func (h *handlers) initialized(_ context.Context, req *mcp.InitializedRequest) {
	p := req.Session.InitializeParams()
	if p == nil {
		return
	}
	negotiated := mcp.SupportedProtocolVersions()[0]
	if slices.Contains(mcp.SupportedProtocolVersions(), p.ProtocolVersion) {
		negotiated = p.ProtocolVersion
	}
	name, ver := "", ""
	if p.ClientInfo != nil {
		name, ver = clip(p.ClientInfo.Name), clip(p.ClientInfo.Version)
	}
	fmt.Fprintf(h.o.Stderr, "git-hint mcp: client %q %q, protocol %s\n", name, ver, negotiated)
	s, err := store.Open(h.o.Dir)
	if err != nil {
		return
	}
	line, _ := json.Marshal(map[string]string{"at": h.o.Now().UTC().Format(time.RFC3339), "client": name,
		"client_version": ver, "requested_protocol": clip(p.ProtocolVersion), "protocol": negotiated, "server_version": version.Version})
	_ = s.AppendLine(line, "state", "mcp.log")
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}
