package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/neprel/git-hint/internal/evals/pipeline"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/project"
	"github.com/neprel/git-hint/internal/snapshot"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/version"
)

type handlers struct{ o Options }

func ptr[T any](v T) *T { return &v }

// Side effect labels (every result carries one).
const (
	effectNone    = "none"
	effectLocal   = "local_write"
	effectNetwork = "network_upload"
)

func (h *handlers) register(s *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	local := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	net := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	add(s, &mcp.Tool{Name: "hint_status", Annotations: ro,
		Description: "Side effect: none. Repositories, last snapshots with digests, activity coverage, server and upload rule, privacy profile."}, h.status)
	add(s, &mcp.Tool{Name: "hint_collect", Annotations: local,
		Description: "Side effect: local write (.hint/ only). Scan the repositories and store a new snapshot when they changed. No network. When the user enabled local evals (git hint eval on), it also builds evals from agent sessions and, only if the user allowed container runs, starts their qualification in containers; this tool cannot change those permissions."}, h.collect)
	add(s, &mcp.Tool{Name: "hint_export_preview", Annotations: ro,
		Description: "Side effect: none. Build and validate the export package under the local policy; report documents, sizes, record counts and fields removed by the policy."}, h.exportPreview)
	add(s, &mcp.Tool{Name: "hint_recommend", Annotations: net,
		Description: "Side effect: network upload to the configured HINT server, only if the user's local rule allows it (otherwise the result is approval_required and nothing is sent). Sends the validated package and fetches the recommendations."}, h.recommend)
	add(s, &mcp.Tool{Name: "hint_plan", Annotations: local,
		Description: "Side effect: local write (one Markdown plan file). Validate a stored recommendation against the current work trees and write the local plan with real file and symbol names. HINT never changes code."}, h.plan)
	add(s, &mcp.Tool{Name: "hint_import_recommendation", Annotations: local,
		Description: "Side effect: local write (stores the document and one Markdown plan). Import a recommendation_set file from inside the project repositories (file mode), validate it like hint_plan, and write the local plan."}, h.importRecommendation)
	add(s, &mcp.Tool{Name: "hint_changes", Annotations: ro,
		Description: "Side effect: none. Changes between the last two snapshots of each repository (local names)."}, h.changes)
	add(s, &mcp.Tool{Name: "hint_evidence", Annotations: ro,
		Description: "Side effect: none. Evidence and uncertainty of a validated recommendation: reason codes, evidence counts, simulation level, calibration, forecasts, local collection coverage."}, h.evidence)
	add(s, &mcp.Tool{Name: "hint_compare", Annotations: ro,
		Description: "Side effect: none. Side-by-side alternatives (no change, context only, refactor, ownership change) when the recommendation supplies more than one; otherwise available=false."}, h.compare)
	h.registerNav(s, ro)
}

func (h *handlers) open() (*store.Store, error) {
	s, err := store.Open(h.o.Dir)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// fail logs the internal error code on stderr and returns the redacted tool error.
func (h *handlers) fail(tool string, err error) error {
	te := classify(err)
	fmt.Fprintf(h.o.Stderr, "git-hint mcp: %s: %s\n", tool, te.Code)
	return te
}

func text(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

// ---------------------------------------------------------------------------
// hint_status

type StatusIn struct{}

type RepoStatus struct {
	Name           string   `json:"name" jsonschema:"local repository name"`
	RepoID         string   `json:"repo_id" jsonschema:"opaque repository ID"`
	SnapshotID     string   `json:"snapshot_id,omitempty"`
	SnapshotDigest string   `json:"snapshot_digest,omitempty"`
	CapturedAt     string   `json:"captured_at,omitempty"`
	Entities       int      `json:"entities"`
	Relations      int      `json:"relations"`
	Languages      []string `json:"languages"`
	WorktreeState  string   `json:"worktree_state,omitempty"`
	SymbolCoverage string   `json:"symbol_coverage" jsonschema:"which languages have declaration-level entities"`
}

type SourceStatus struct {
	Agent        string   `json:"agent"`
	Adapter      string   `json:"adapter"`
	Capabilities []string `json:"capabilities"`
	Unavailable  []string `json:"unavailable"`
}

type StatusOut struct {
	SideEffect       string         `json:"side_effect"`
	SchemaVersion    string         `json:"schema_version"`
	ClientVersion    string         `json:"client_version"`
	ProjectNamespace string         `json:"project_namespace"`
	Repositories     []RepoStatus   `json:"repositories"`
	Observations     int            `json:"observations" jsonschema:"spooled activity observations"`
	Sources          []SourceStatus `json:"sources"`
	ServerConfigured bool           `json:"server_configured"`
	MCPUpload        string         `json:"mcp_upload" jsonschema:"deny or allow: the user's local rule for hint_recommend"`
	Profile          string         `json:"privacy_profile"`
	TimeResolution   int            `json:"time_resolution_seconds"`
	RecordTypes      []string       `json:"export_record_types"`
	Queued           int            `json:"queued_packages"`
	Missing          []string       `json:"missing" jsonschema:"collection capabilities that this version does not have"`
}

var missingCapabilities = []string{
	"declarations for languages other than Go (file level only)",
	"call graph and data flow",
	"task and turn IDs and token usage from the OpenCode plugin",
	"adapters for Claude Code and Codex",
}

func uploadRule(c store.Config) string {
	if c.MCPUploadAllowed() {
		return "allow"
	}
	return "deny"
}

func (h *handlers) status(ctx context.Context, _ *mcp.CallToolRequest, _ StatusIn) (*mcp.CallToolResult, StatusOut, error) {
	s, err := h.open()
	if err != nil {
		return nil, StatusOut{}, h.fail("hint_status", err)
	}
	out := StatusOut{SideEffect: effectNone, SchemaVersion: version.SpecVersion, ClientVersion: version.Version,
		ProjectNamespace: s.Cfg.ProjectNamespace, ServerConfigured: s.Cfg.ServerURL != "", MCPUpload: uploadRule(s.Cfg),
		Profile: s.Cfg.Policy.Profile, TimeResolution: s.Cfg.Policy.TimeResolutionSeconds, RecordTypes: s.Cfg.Policy.RecordTypes,
		Missing: missingCapabilities, Repositories: []RepoStatus{}, Sources: []SourceStatus{}}
	for _, r := range s.Cfg.Repos {
		rs := RepoStatus{Name: r.Name, RepoID: s.RepoID(r), Languages: []string{}, SymbolCoverage: "go"}
		if l, err := project.LoadLatest(s, rs.RepoID); err == nil && l != nil {
			rs.SnapshotID, rs.SnapshotDigest, rs.CapturedAt = l.Record.SnapshotID, l.Record.Digest, l.Record.CapturedAt
			rs.Entities, rs.Relations, rs.Languages, rs.WorktreeState = len(l.Entities), len(l.Relations), l.Record.Languages, l.Record.WorktreeState
		}
		out.Repositories = append(out.Repositories, rs)
	}
	if obs, _, err := project.ReadSpool(s); err == nil {
		out.Observations = len(obs)
	}
	if srcs, err := project.Sources(s); err == nil {
		for _, x := range srcs {
			out.Sources = append(out.Sources, SourceStatus{Agent: x.Agent, Adapter: x.Adapter.Name, Capabilities: x.Capabilities, Unavailable: x.Unavailable})
		}
		sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].Adapter < out.Sources[j].Adapter })
	}
	if q, err := project.Queued(s); err == nil {
		out.Queued = len(q)
	}
	return text("%d repositories, %d observations, server configured: %v, MCP upload rule: %s.",
		len(out.Repositories), out.Observations, out.ServerConfigured, out.MCPUpload), out, nil
}

// ---------------------------------------------------------------------------
// hint_collect

type CollectIn struct {
	Full   bool   `json:"full,omitempty" jsonschema:"ignore the scan cache (full baseline)"`
	TaskID string `json:"task_id,omitempty" jsonschema:"task identifier from the agent or orchestrator, if it has one; stored only as an opaque ID; never invent one"`
}

type CollectRepo struct {
	Name           string `json:"name"`
	SnapshotID     string `json:"snapshot_id"`
	SnapshotDigest string `json:"snapshot_digest"`
	Changed        bool   `json:"changed"`
	Files          int    `json:"files"`
	Parsed         int    `json:"parsed"`
	Reused         int    `json:"reused_from_cache"`
	Entities       int    `json:"entities"`
	Relations      int    `json:"relations"`
	FileChanges    int    `json:"file_changes"`
	ParseErrors    int    `json:"go_parse_errors"`
	DurationMS     int64  `json:"duration_ms"`
}

type CollectOut struct {
	SideEffect    string            `json:"side_effect"`
	Evals         *pipeline.Summary `json:"evals,omitempty"` // only when the user's eval policy is on
	SchemaVersion string            `json:"schema_version"`
	Repositories  []CollectRepo     `json:"repositories"`
	Missing       []string          `json:"missing"`
}

func (h *handlers) collect(ctx context.Context, _ *mcp.CallToolRequest, in CollectIn) (*mcp.CallToolResult, CollectOut, error) {
	if len(in.TaskID) > 200 {
		return nil, CollectOut{}, h.fail("hint_collect", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, CollectOut{}, h.fail("hint_collect", err)
	}
	lock, err := s.LockState(false)
	if err != nil {
		return nil, CollectOut{}, h.fail("hint_collect", err)
	}
	defer lock.Unlock()
	ctx, cancel := withTimeout(ctx, h.o.CollectTimeout)
	defer cancel()
	res, err := project.Collect(s, project.CollectOptions{Full: in.Full, Now: h.o.Now, Ctx: ctx, TaskID: in.TaskID})
	if err != nil {
		return nil, CollectOut{}, h.fail("hint_collect", err)
	}
	out := CollectOut{SideEffect: effectLocal, SchemaVersion: version.SpecVersion, Missing: missingCapabilities}
	changed := 0
	for _, r := range res {
		if r.Changed {
			changed++
		}
		out.Repositories = append(out.Repositories, CollectRepo{Name: r.Name, SnapshotID: r.Snapshot.Record.SnapshotID,
			SnapshotDigest: r.Snapshot.Record.Digest, Changed: r.Changed, Files: r.Scan.Files, Parsed: r.Scan.Parsed,
			Reused: r.Scan.Reused, Entities: len(r.Snapshot.Entities), Relations: len(r.Snapshot.Relations),
			FileChanges: r.Mutations, ParseErrors: r.Scan.Unparsed, DurationMS: r.Duration.Milliseconds()})
	}
	lock.Unlock()
	env := pipeline.DefaultEnv
	if h.o.EvalEnv != nil {
		env = h.o.EvalEnv
	}
	spawn := func() error { return pipeline.SpawnWorker(s) }
	if h.o.Spawn != nil {
		spawn = h.o.Spawn
	}
	out.Evals = pipeline.AfterCollect(ctx, s, env(), spawn)
	return text("Collected %d repositories; %d new snapshots. No data left the machine.", len(res), changed), out, nil
}

// ---------------------------------------------------------------------------
// hint_export_preview

type PreviewIn struct{}

type DocInfo struct {
	Type  string `json:"type"`
	File  string `json:"file"`
	Bytes int    `json:"bytes"`
}

type PreviewOut struct {
	SideEffect     string         `json:"side_effect"`
	SchemaVersion  string         `json:"schema_version"`
	Valid          bool           `json:"valid"`
	Documents      []DocInfo      `json:"documents"`
	TotalBytes     int            `json:"total_bytes"`
	Records        map[string]int `json:"records"`
	RemovedFields  map[string]int `json:"removed_fields" jsonschema:"fields removed by the local export policy, per record type"`
	Profile        string         `json:"privacy_profile"`
	TimeResolution int            `json:"time_resolution_seconds"`
	PolicyID       string         `json:"policy_id"`
	ExactFeatures  []string       `json:"exact_features"`
	Notes          []string       `json:"notes"`
}

func (h *handlers) exportPreview(ctx context.Context, _ *mcp.CallToolRequest, _ PreviewIn) (*mcp.CallToolResult, PreviewOut, error) {
	s, err := h.open()
	if err != nil {
		return nil, PreviewOut{}, h.fail("hint_export_preview", err)
	}
	p, err := project.BuildPackage(s, project.ExportOptions{Now: h.o.Now()})
	if err != nil {
		return nil, PreviewOut{}, h.fail("hint_export_preview", err)
	}
	out := PreviewOut{SideEffect: effectNone, SchemaVersion: version.SpecVersion, Valid: true, Records: p.Report.Records,
		RemovedFields: p.Report.RemovedFields, Profile: p.Report.Profile, TimeResolution: p.Report.TimeResolutionSeconds,
		PolicyID: p.Report.PolicyID, ExactFeatures: p.Report.Features, Notes: p.Report.Notes}
	if out.ExactFeatures == nil {
		out.ExactFeatures = []string{}
	}
	for _, d := range p.Docs {
		out.Documents = append(out.Documents, DocInfo{Type: d.Type, File: d.File, Bytes: len(d.Raw)})
		out.TotalBytes += len(d.Raw)
	}
	return text("The package is valid: %d documents, %d bytes, profile %s. Nothing was written or sent.",
		len(out.Documents), out.TotalBytes, out.Profile), out, nil
}

// ---------------------------------------------------------------------------
// hint_recommend

type RecommendIn struct{}

type Forecast struct {
	Metric   string   `json:"metric"`
	Unit     string   `json:"unit"`
	Per      string   `json:"per"`
	Lower    float64  `json:"lower"`
	Upper    float64  `json:"upper"`
	Level    float64  `json:"level"`
	Point    *float64 `json:"point,omitempty"`
	Kind     string   `json:"kind" jsonschema:"always forecast: a prediction, not a measured effect"`
	SimLevel string   `json:"simulation_level,omitempty"`
}

type PlanSummary struct {
	PlanID            string     `json:"plan_id"`
	Status            string     `json:"status" jsonschema:"accept, partial, render_only or reject (local import validation)"`
	Codes             []string   `json:"codes"`
	InterventionClass string     `json:"intervention_class"`
	Transformation    string     `json:"transformation,omitempty"`
	Decision          string     `json:"decision"`
	AffectedRepos     []string   `json:"affected_repositories" jsonschema:"local names of the repositories the plan names"`
	UnresolvedRepos   []string   `json:"unresolved_repositories" jsonschema:"repository IDs of the plan that are not in this project"`
	Forecasts         []Forecast `json:"forecasts"`
	Steps             int        `json:"steps"`
	RequiresApproval  bool       `json:"requires_approval"`
}

type RecommendOut struct {
	SideEffect       string        `json:"side_effect"`
	Status           string        `json:"status" jsonschema:"approval_required, sent"`
	Message          string        `json:"message"`
	DocumentsSent    int           `json:"documents_sent"`
	ServerOrigin     string        `json:"server_origin,omitempty"`
	RecommendationID string        `json:"recommendation_id,omitempty"`
	DocumentResult   string        `json:"document_result,omitempty"`
	Plans            []PlanSummary `json:"plans"`
	Note             string        `json:"note"`
}

const notAcceptance = "A fetched recommendation is not an accepted change. Read the plan from hint_plan, review it, and run the checks before any code change."

func (h *handlers) recommend(ctx context.Context, _ *mcp.CallToolRequest, _ RecommendIn) (*mcp.CallToolResult, RecommendOut, error) {
	s, err := h.open()
	if err != nil {
		return nil, RecommendOut{}, h.fail("hint_recommend", err)
	}
	out := RecommendOut{SideEffect: effectNone, Plans: []PlanSummary{}, Note: notAcceptance}
	if !s.Cfg.MCPUploadAllowed() {
		out.Status = "approval_required"
		out.Message = "The user's local rule does not allow uploads from MCP. Nothing was sent. The user can inspect the package with `git hint export --dry-run --privacy-report` and send it with `git hint recommend`, or allow MCP uploads with `git hint init --mcp-upload allow`."
		return text("Approval required: nothing was sent."), out, nil
	}
	if s.Cfg.ServerURL == "" {
		return nil, out, h.fail("hint_recommend", toolErr("server_unconfigured"))
	}
	c, err := h.o.NewClient(s.Cfg.ServerURL) // the configured server only; never an argument or env value
	if err != nil {
		return nil, out, h.fail("hint_recommend", toolErr("server_unconfigured"))
	}
	lock, err := s.LockState(false)
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	defer lock.Unlock()
	ctx, cancel := withTimeout(ctx, h.o.NetworkTimeout)
	defer cancel()
	now := h.o.Now()
	p, err := project.BuildPackage(s, project.ExportOptions{Now: now})
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	if err := project.Send(ctx, s, c, p, now); err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	out.SideEffect, out.Status, out.DocumentsSent, out.ServerOrigin = effectNetwork, "sent", len(p.Docs), c.Origin()
	raw, err := c.Recommendations(ctx, s.Cfg.ProjectNamespace)
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	path, err := project.SaveRecommendation(s, raw)
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	cur, err := project.CurrentStateCtx(ctx, s, now)
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	im, err := project.Import(raw, cur)
	if err != nil {
		return nil, out, h.fail("hint_recommend", err)
	}
	out.RecommendationID = docID(path)
	out.DocumentResult = im.Result.Document
	out.Plans = summarize(im, cur)
	out.Message = fmt.Sprintf("Sent %d validated documents to the configured server and fetched the recommendations.", out.DocumentsSent)
	return text("%s Document check: %s; %d plans. %s", out.Message, out.DocumentResult, len(out.Plans), notAcceptance), out, nil
}

func docID(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var head struct {
		ID string `json:"document_id"`
	}
	if json.Unmarshal(b, &head) != nil {
		return ""
	}
	return head.ID
}

func summarize(im *project.Imported, cur *project.Current) []PlanSummary {
	out := []PlanSummary{}
	if im.Doc == nil {
		return out
	}
	status := map[string]hint.PlanResult{}
	for _, r := range im.Result.Plans {
		status[r.PlanID] = r
	}
	plans, _ := im.Doc["payload"].(map[string]any)["plans"].([]any)
	for _, x := range plans {
		m := x.(map[string]any)
		id, _ := m["plan_id"].(string)
		ps := PlanSummary{PlanID: id, Status: status[id].Status, Codes: status[id].Codes, AffectedRepos: []string{},
			UnresolvedRepos: []string{}, Forecasts: forecasts(m)}
		if ps.Codes == nil {
			ps.Codes = []string{}
		}
		ps.InterventionClass, _ = m["intervention_class"].(string)
		ps.Transformation, _ = m["transformation"].(string)
		ps.Decision, _ = m["decision"].(string)
		for _, r := range anyList(m["repo_ids"]) {
			rid, _ := r.(string)
			if cr, ok := cur.Repos[rid]; ok {
				ps.AffectedRepos = append(ps.AffectedRepos, cr.Name)
			} else {
				ps.UnresolvedRepos = append(ps.UnresolvedRepos, rid)
			}
		}
		steps := anyList(m["steps"])
		ps.Steps = len(steps)
		for _, st := range steps {
			if sm, _ := st.(map[string]any); sm["requires_approval"] == true {
				ps.RequiresApproval = true
			}
		}
		if len(anyList(m["required_approvals"])) > 0 {
			ps.RequiresApproval = true
		}
		out = append(out, ps)
	}
	return out
}

func forecasts(m map[string]any) []Forecast {
	out := []Forecast{}
	sim := ""
	if s, ok := m["simulation"].(map[string]any); ok {
		sim, _ = s["level"].(string)
	}
	for _, x := range anyList(m["predictions"]) {
		p, _ := x.(map[string]any)
		iv, _ := p["interval"].(map[string]any)
		f := Forecast{Kind: "forecast", SimLevel: sim}
		f.Metric, _ = p["metric"].(string)
		f.Unit, _ = p["unit"].(string)
		f.Per, _ = p["per"].(string)
		f.Lower, _ = iv["lower"].(float64)
		f.Upper, _ = iv["upper"].(float64)
		f.Level, _ = iv["level"].(float64)
		if v, ok := p["point"].(float64); ok {
			f.Point = &v
		}
		out = append(out, f)
	}
	return out
}

func anyList(v any) []any { l, _ := v.([]any); return l }

// ---------------------------------------------------------------------------
// hint_plan and hint_import_recommendation

type PlanIn struct {
	RecommendationID string `json:"recommendation_id,omitempty" jsonschema:"document ID from hint_recommend or hint_import_recommendation; default latest"`
	OutputPath       string `json:"output_path,omitempty" jsonschema:"Markdown file inside a project repository (relative to the project root); default inside .hint/state/recommendations/"`
}

type PlanOut struct {
	SideEffect       string        `json:"side_effect"`
	SchemaVersion    string        `json:"schema_version"`
	RecommendationID string        `json:"recommendation_id"`
	DocumentResult   string        `json:"document_result"`
	Plans            []PlanSummary `json:"plans"`
	MarkdownPath     string        `json:"markdown_path"`
	Markdown         string        `json:"markdown" jsonschema:"the local plan (may be truncated; the file is complete)"`
	Truncated        bool          `json:"truncated"`
	Note             string        `json:"note"`
}

func (h *handlers) plan(ctx context.Context, _ *mcp.CallToolRequest, in PlanIn) (*mcp.CallToolResult, PlanOut, error) {
	if len(in.RecommendationID) > 64 || len(in.OutputPath) > 4096 {
		return nil, PlanOut{}, h.fail("hint_plan", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_plan", err)
	}
	src, err := project.FindRecommendation(s, in.RecommendationID)
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_plan", toolErr("recommendation_missing"))
	}
	return h.renderPlan(ctx, s, "hint_plan", src, in.OutputPath)
}

type ImportIn struct {
	Path       string `json:"path" jsonschema:"recommendation_set file inside a project repository (relative to the project root or absolute)"`
	OutputPath string `json:"output_path,omitempty" jsonschema:"Markdown file inside a project repository; default inside .hint/state/recommendations/"`
}

func (h *handlers) importRecommendation(ctx context.Context, _ *mcp.CallToolRequest, in ImportIn) (*mcp.CallToolResult, PlanOut, error) {
	if in.Path == "" || len(in.Path) > 4096 || len(in.OutputPath) > 4096 {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", err)
	}
	src, ok := insideWorkspace(s, in.Path)
	if !ok {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", toolErr("path_not_allowed"))
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", toolErr("invalid_argument"))
	}
	if fi.Size() > project.MaxDocumentBytes {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", toolErr("too_large"))
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", toolErr("invalid_argument"))
	}
	lock, err := s.LockState(false)
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", err)
	}
	stored, err := project.SaveRecommendation(s, raw)
	lock.Unlock()
	if err != nil {
		return nil, PlanOut{}, h.fail("hint_import_recommendation", err)
	}
	return h.renderPlan(ctx, s, "hint_import_recommendation", stored, in.OutputPath)
}

// renderPlan is the one validation and rendering path of hint_plan and
// hint_import_recommendation (the same as `git hint plan`).
func (h *handlers) renderPlan(ctx context.Context, s *store.Store, tool, src, output string) (*mcp.CallToolResult, PlanOut, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, PlanOut{}, h.fail(tool, toolErr("recommendation_missing"))
	}
	ctx, cancel := withTimeout(ctx, h.o.CollectTimeout)
	defer cancel()
	now := h.o.Now()
	cur, err := project.CurrentStateCtx(ctx, s, now)
	if err != nil {
		return nil, PlanOut{}, h.fail(tool, err)
	}
	im, err := project.Import(raw, cur)
	if err != nil {
		return nil, PlanOut{}, h.fail(tool, err)
	}
	md := project.Render(im, cur, filepath.Base(src))
	dst, err := h.planPath(s, output, now.UTC().Format("20060102T150405.000Z"))
	if err != nil {
		return nil, PlanOut{}, h.fail(tool, err)
	}
	lock, err := s.LockState(false)
	if err != nil {
		return nil, PlanOut{}, h.fail(tool, err)
	}
	defer lock.Unlock()
	if err := writePlan(s, dst, []byte(md)); err != nil {
		return nil, PlanOut{}, h.fail(tool, err)
	}
	out := PlanOut{SideEffect: effectLocal, SchemaVersion: version.SpecVersion, RecommendationID: docID(src),
		DocumentResult: im.Result.Document, Plans: summarize(im, cur), MarkdownPath: dst, Markdown: md, Note: notAcceptance}
	if len(out.Markdown) > h.o.MaxMarkdownBytes {
		out.Markdown, out.Truncated = out.Markdown[:h.o.MaxMarkdownBytes], true
	}
	accepted := 0
	for _, p := range out.Plans {
		if p.Status == "accept" || p.Status == "partial" {
			accepted++
		}
	}
	return text("Document check: %s; %d plans, %d applicable. Plan file: %s. %s", out.DocumentResult, len(out.Plans),
		accepted, dst, notAcceptance), out, nil
}

// planPath checks the output path: default inside .hint/state/recommendations/, else a
// .md file inside a project repository, not below .git/ or .hint/, with no symbolic link
// in the path (a dangling link included), and either new or an earlier HINT plan.
func (h *handlers) planPath(s *store.Store, output, stamp string) (string, error) {
	if output == "" {
		return s.Path("state", "recommendations", "plan-"+stamp+".md"), nil
	}
	p, ok := insideWorkspace(s, output)
	if !ok || !strings.HasSuffix(strings.ToLower(p), ".md") || hasSymlink(p) {
		return "", toolErr("path_not_allowed")
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".git" || part == store.Dir {
			return "", toolErr("path_not_allowed")
		}
	}
	fi, err := os.Lstat(p)
	if err == nil {
		if !fi.Mode().IsRegular() {
			return "", toolErr("path_not_allowed")
		}
		if b, err := os.ReadFile(p); err != nil || !strings.HasPrefix(string(b), "# HINT recommendation plan") {
			return "", toolErr("path_not_allowed")
		}
	} else if !os.IsNotExist(err) {
		return "", toolErr("path_not_allowed")
	}
	return p, nil
}

// hasSymlink reports whether p or one of its existing parent directories is a symbolic
// link. insideWorkspace resolves the existing part of the path, so any link found here is
// one that could not be resolved, such as a dangling link.
func hasSymlink(p string) bool {
	for cur := p; ; cur = filepath.Dir(cur) {
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

// writePlan writes a plan outside the store without following links: it creates the
// missing directories, checks again that the directory has no symbolic link, writes a new
// temporary file (exclusive create) and renames it over the destination. A rename
// replaces a link at the destination; it never writes through it.
func writePlan(s *store.Store, dst string, md []byte) error {
	if rel, err := filepath.Rel(s.Path(), dst); err == nil && !strings.HasPrefix(rel, "..") {
		return s.WriteState(md, strings.Split(filepath.ToSlash(rel), "/")...)
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if hasSymlink(dir) {
		return toolErr("path_not_allowed")
	}
	f, err := os.CreateTemp(dir, ".hint-plan-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(md)
	if err := f.Chmod(0o644); werr == nil {
		werr = err
	}
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr == nil {
		werr = os.Rename(tmp, dst)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// insideWorkspace resolves p (relative to the project root) and checks that it lies in a
// project repository, after symbolic links are resolved.
func insideWorkspace(s *store.Store, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Root, p)
	}
	p = filepath.Clean(p)
	resolved := resolveExisting(p)
	for _, r := range s.Cfg.Repos {
		for _, root := range []string{filepath.Clean(s.RepoRoot(r)), resolveExisting(s.RepoRoot(r))} {
			if rel, err := filepath.Rel(root, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return resolved, true
			}
		}
	}
	return "", false
}

func resolveExisting(p string) string {
	rest := ""
	cur := p
	for {
		if ev, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(ev, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// ---------------------------------------------------------------------------
// hint_changes

type ChangesIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum changes per repository (1-500, default 100)"`
}

type ChangeItem struct {
	Operation   string   `json:"operation"`
	Level       string   `json:"level"`
	Before      []string `json:"before"`
	After       []string `json:"after"`
	MatchMethod string   `json:"match_method"`
	Confidence  *float64 `json:"confidence,omitempty"`
}

type ChangesRepo struct {
	Name      string         `json:"name"`
	From      string         `json:"from_snapshot,omitempty"`
	To        string         `json:"to_snapshot,omitempty"`
	Counts    map[string]int `json:"counts"`
	Changes   []ChangeItem   `json:"changes"`
	Truncated bool           `json:"truncated"`
}

type ChangesOut struct {
	SideEffect   string        `json:"side_effect"`
	Repositories []ChangesRepo `json:"repositories"`
}

func (h *handlers) changes(ctx context.Context, _ *mcp.CallToolRequest, in ChangesIn) (*mcp.CallToolResult, ChangesOut, error) {
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return nil, ChangesOut{}, h.fail("hint_changes", toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, ChangesOut{}, h.fail("hint_changes", err)
	}
	out := ChangesOut{SideEffect: effectNone, Repositories: []ChangesRepo{}}
	total := 0
	for _, r := range s.Cfg.Repos {
		cr := ChangesRepo{Name: r.Name, Counts: map[string]int{}, Changes: []ChangeItem{}}
		cur, err := project.LoadLatest(s, s.RepoID(r))
		if err != nil {
			return nil, out, h.fail("hint_changes", err)
		}
		if cur != nil && len(cur.Record.ParentSnapshotIDs) > 0 {
			prev, err := project.LoadSnapshot(s, cur.Record.RepoID, cur.Record.ParentSnapshotIDs[0])
			if err == nil {
				_, local := snapshot.Diff(s.Key(), prev, cur)
				cr.From, cr.To = prev.Record.SnapshotID, cur.Record.SnapshotID
				for _, c := range local {
					cr.Counts[c.Operation+"/"+c.Level]++
					if len(cr.Changes) >= limit {
						cr.Truncated = true
						continue
					}
					cr.Changes = append(cr.Changes, ChangeItem{Operation: c.Operation, Level: c.Level, Before: nonNil(c.BeforeNames),
						After: nonNil(c.AfterNames), MatchMethod: c.MatchMethod, Confidence: c.MatchConfidence})
				}
				total += len(local)
			}
		}
		out.Repositories = append(out.Repositories, cr)
	}
	return text("%d changes between the last two snapshots.", total), out, nil
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// ---------------------------------------------------------------------------
// hint_evidence and hint_compare

type RecIn struct {
	RecommendationID string `json:"recommendation_id,omitempty" jsonschema:"document ID; default latest"`
}

type Reason struct {
	Code         string `json:"code"`
	Observations int    `json:"observations"`
	Metrics      int    `json:"metrics"`
}

type Simulation struct {
	Level             string `json:"level"`
	CalibrationStatus string `json:"calibration_status"`
	OutOfDistribution *bool  `json:"out_of_distribution,omitempty"`
}

type PlanEvidence struct {
	PlanID     string      `json:"plan_id"`
	Status     string      `json:"status"`
	Reasons    []Reason    `json:"reasons"`
	Simulation *Simulation `json:"simulation,omitempty"`
	Forecasts  []Forecast  `json:"forecasts"`
	Warnings   []string    `json:"warnings"`
}

type EvidenceOut struct {
	SideEffect       string         `json:"side_effect"`
	RecommendationID string         `json:"recommendation_id"`
	DocumentResult   string         `json:"document_result"`
	Plans            []PlanEvidence `json:"plans"`
	LocalCoverage    []SourceStatus `json:"local_coverage"`
}

func (h *handlers) loadValidated(ctx context.Context, tool, id string) (*store.Store, *project.Imported, *project.Current, string, error) {
	if len(id) > 64 {
		return nil, nil, nil, "", h.fail(tool, toolErr("invalid_argument"))
	}
	s, err := h.open()
	if err != nil {
		return nil, nil, nil, "", h.fail(tool, err)
	}
	src, err := project.FindRecommendation(s, id)
	if err != nil {
		return nil, nil, nil, "", h.fail(tool, toolErr("recommendation_missing"))
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, nil, nil, "", h.fail(tool, toolErr("recommendation_missing"))
	}
	ctx, cancel := withTimeout(ctx, h.o.CollectTimeout)
	defer cancel()
	cur, err := project.CurrentStateCtx(ctx, s, h.o.Now())
	if err != nil {
		return nil, nil, nil, "", h.fail(tool, err)
	}
	im, err := project.Import(raw, cur)
	if err != nil {
		return nil, nil, nil, "", h.fail(tool, err)
	}
	return s, im, cur, docID(src), nil
}

func (h *handlers) evidence(ctx context.Context, _ *mcp.CallToolRequest, in RecIn) (*mcp.CallToolResult, EvidenceOut, error) {
	s, im, cur, id, err := h.loadValidated(ctx, "hint_evidence", in.RecommendationID)
	if err != nil {
		return nil, EvidenceOut{}, err
	}
	out := EvidenceOut{SideEffect: effectNone, RecommendationID: id, DocumentResult: im.Result.Document, Plans: []PlanEvidence{}, LocalCoverage: []SourceStatus{}}
	if srcs, err := project.Sources(s); err == nil {
		for _, x := range srcs {
			out.LocalCoverage = append(out.LocalCoverage, SourceStatus{Agent: x.Agent, Adapter: x.Adapter.Name, Capabilities: x.Capabilities, Unavailable: x.Unavailable})
		}
		sort.Slice(out.LocalCoverage, func(i, j int) bool { return out.LocalCoverage[i].Adapter < out.LocalCoverage[j].Adapter })
	}
	if im.Doc == nil {
		return text("Document check: %s. No evidence is shown for a rejected document.", im.Result.Document), out, nil
	}
	sums := summarize(im, cur)
	for i, x := range anyList(im.Doc["payload"].(map[string]any)["plans"]) {
		m := x.(map[string]any)
		pe := PlanEvidence{PlanID: sums[i].PlanID, Status: sums[i].Status, Reasons: []Reason{}, Forecasts: sums[i].Forecasts, Warnings: []string{}}
		for _, r := range anyList(m["reasons"]) {
			rm, _ := r.(map[string]any)
			code, _ := rm["code"].(string)
			pe.Reasons = append(pe.Reasons, Reason{Code: code, Observations: len(anyList(rm["observation_ids"])), Metrics: len(anyList(rm["metric_ids"]))})
		}
		if sm, ok := m["simulation"].(map[string]any); ok {
			sim := &Simulation{CalibrationStatus: "not given"}
			sim.Level, _ = sm["level"].(string)
			if c, ok := sm["calibration_status"].(string); ok {
				sim.CalibrationStatus = c
			}
			if o, ok := sm["out_of_distribution"].(bool); ok {
				sim.OutOfDistribution = &o
				if o {
					pe.Warnings = append(pe.Warnings, "out of distribution: no calibration data for this task class or agent configuration")
				}
			}
			if sim.CalibrationStatus == "uncalibrated" {
				pe.Warnings = append(pe.Warnings, "uncalibrated forecast")
			}
			pe.Simulation = sim
		} else if len(pe.Forecasts) > 0 {
			pe.Warnings = append(pe.Warnings, "forecast without simulation details")
		}
		if len(pe.Reasons) == 0 {
			pe.Warnings = append(pe.Warnings, "no reason codes")
		}
		out.Plans = append(out.Plans, pe)
	}
	return text("Evidence for %d plans. Forecasts are predictions, not measured effects.", len(out.Plans)), out, nil
}

type Alternative struct {
	PlanID            string     `json:"plan_id"`
	Status            string     `json:"status"`
	InterventionClass string     `json:"intervention_class"`
	Transformation    string     `json:"transformation,omitempty"`
	Decision          string     `json:"decision"`
	Forecasts         []Forecast `json:"forecasts"`
}

type CompareOut struct {
	SideEffect       string        `json:"side_effect"`
	RecommendationID string        `json:"recommendation_id"`
	Available        bool          `json:"available"`
	Reason           string        `json:"reason,omitempty"`
	Alternatives     []Alternative `json:"alternatives"`
}

func (h *handlers) compare(ctx context.Context, _ *mcp.CallToolRequest, in RecIn) (*mcp.CallToolResult, CompareOut, error) {
	_, im, cur, id, err := h.loadValidated(ctx, "hint_compare", in.RecommendationID)
	if err != nil {
		return nil, CompareOut{}, err
	}
	out := CompareOut{SideEffect: effectNone, RecommendationID: id, Alternatives: []Alternative{}}
	if im.Doc == nil {
		out.Reason = "the recommendation document was rejected (" + im.Result.Document + ")"
		return text("Comparison unavailable: %s.", out.Reason), out, nil
	}
	classes := map[string]bool{}
	for _, p := range summarize(im, cur) {
		classes[p.InterventionClass] = true
		out.Alternatives = append(out.Alternatives, Alternative{PlanID: p.PlanID, Status: p.Status, InterventionClass: p.InterventionClass,
			Transformation: p.Transformation, Decision: p.Decision, Forecasts: p.Forecasts})
	}
	if len(classes) < 2 {
		out.Available, out.Reason = false, "the recommendation offers only one intervention class"
		return text("Comparison unavailable: %s.", out.Reason), out, nil
	}
	out.Available = true
	return text("%d alternatives in %d intervention classes. Forecasts are predictions, not measured effects.", len(out.Alternatives), len(classes)), out, nil
}
