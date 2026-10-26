package runner

// The policy bridge describes installed procedure data; it does not admit producer
// authority. Its production catalogue is empty. No caller can construct private options.
import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

const ReferencePreparationPolicyVersion = "hint-reference-prepare-policy-v1"

var ErrReferencePreparationPolicy = errors.New("reference_preparation_policy_refused")

//go:embed task_result.go frozen_inputs.go frozen_bridge.go bounded_phase.go frozen_io.go agentrun.go capture.go capture_ssh.go docker.go export.go gate.go io.go launch_recipe.go ledger.go live.go live_docker.go opencode.go prelaunch.go prelaunch_input_unix.go prelaunch_input_other.go provenance.go reference_policy.go reference_policy_catalogue.go reference_policy_other.go reference_policy_unix.go reference_prepare.go ripgrep.go ripgrep_unix.go ripgrep_other.go run.go study_inspection.go tree_archive.go trust.go reference_policy_dependencies.json
var referencePolicySources embed.FS

type ReferencePreparationPolicyRef struct{ PolicyID, PolicyDigest string }
type ReferencePolicyPrepareRequest struct {
	Policy    ReferencePreparationPolicyRef
	OutputDir string
	Expected  ReferenceInputIdentity
}
type ReferencePolicySource struct {
	Path, Digest string
	Bytes        int64
}
type ReferencePolicyBounds struct {
	Operation, Resolve, Commit, Build, Apply, Overhead, Cleanup time.Duration
	ArchiveBytes, TotalBytes                                    int64
}
type ReferencePolicyExecutionRules struct {
	Interface, ArtifactDomain, RuntimeNetwork, BuildNetwork string
	OracleRuns                                              int
	CPUsNano, MemoryBytes, PidsLimit                        int64
}
type ReferencePreparationPolicyInfo struct {
	Rules                                                               ReferencePolicyExecutionRules
	Version, Authority                                                  string
	Policy                                                              ReferencePreparationPolicyRef
	BundleDigest, CatalogueDigest, EndpointDigest, SocketIdentityDigest string
	EngineRef, EngineVersion, EngineOS, EngineArchitecture, APIVersion  string
	Bounds                                                              ReferencePolicyBounds
	Sources                                                             []ReferencePolicySource
	// Dependencies are exact declared source/module pins, not a complete transitive
	// compiled-source closure or source-to-binary attestation.
	Dependencies []ReferencePolicySource
}
type referenceSocketPin struct {
	Size, MTimeSec, MTimeNS, CTimeSec, CTimeNS int64
	Device, Inode                              uint64
	Mode, UID, GID                             uint32
}

const preparationPurpose = "reference_preparation"
const frozenPurpose = "frozen_b1_verification"

func entryPurpose(p referencePolicyEntry) string {
	if p.Purpose == "" {
		return preparationPurpose
	}
	return p.Purpose
}

type referencePolicyEntry struct {
	Purpose, Image                                                     string
	ID, BundleDigest, Endpoint                                         string
	Socket                                                             referenceSocketPin
	EngineRef, EngineVersion, EngineOS, EngineArchitecture, APIVersion string
	Procedure                                                          referencePreparationPolicy
}
type referenceEngineBinding struct {
	Purpose                                                            string
	EngineRef, EngineVersion, EngineOS, EngineArchitecture, APIVersion string
	Policy                                                             ReferencePreparationPolicyRef
	BundleDigest, EndpointDigest, SocketDigest                         string
}
type referencePolicyError struct{ code string }

func (e referencePolicyError) Error() string {
	return ErrReferencePreparationPolicy.Error() + ": " + e.code
}
func (e referencePolicyError) Unwrap() error   { return ErrReferencePreparationPolicy }
func referencePolicyFailure(code string) error { return referencePolicyError{code} }

var referencePolicyID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
var referencePolicySHA = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func referenceBounds(p referencePreparationPolicy) ReferencePolicyBounds {
	return ReferencePolicyBounds{p.operation, p.resolve, p.commit, p.build, p.apply, p.overhead, p.cleanup, p.archiveCap, p.totalCap}
}
func referenceEntryDigest(p referencePolicyEntry) string {
	// The caller's exact policy digest covers catalogue source too. The installed entry
	// stores no policy digest, so code/data identities have no self-hash cycle.
	catalogue, _ := referencePolicySources.ReadFile("reference_policy_catalogue.go")
	base := referenceJSON(struct {
		Domain, ID, Bundle, CatalogueSource, Endpoint string
		Socket                                        referenceSocketPin
		Engine, Version, OS, Architecture, API        string
		Bounds                                        ReferencePolicyBounds
	}{ReferencePreparationPolicyVersion, p.ID, p.BundleDigest, referenceDigest(catalogue), p.Endpoint, p.Socket, p.EngineRef, p.EngineVersion, p.EngineOS, p.EngineArchitecture, p.APIVersion, referenceBounds(p.Procedure)})
	if entryPurpose(p) == frozenPurpose {
		return referenceJSON(struct{ Purpose, Image, Binding string }{frozenPurpose, p.Image, base})
	}
	return base
}

// SourceBundle covers embedded runner Go sources and the exact declared source/module
// pins, plus the actual Go toolchain/platform. The source-set guard includes both
// platform variants. These pins do not establish a complete transitive compiled-source
// closure or source-to-binary correspondence. Catalogue data is separately bound to
// avoid a self-hash cycle; none of these identities is attestation.
func referenceBundleIdentity() (string, []ReferencePolicySource, []ReferencePolicySource, error) {
	entries, e := referencePolicySources.ReadDir(".")
	if e != nil {
		return "", nil, nil, referencePolicyFailure("bundle_invalid")
	}
	sources := []ReferencePolicySource{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "reference_policy_catalogue.go" {
			continue
		}
		b, e := referencePolicySources.ReadFile(name)
		if e != nil {
			return "", nil, nil, referencePolicyFailure("bundle_invalid")
		}
		sources = append(sources, ReferencePolicySource{name, referenceDigest(b), int64(len(b))})
	}
	raw, e := referencePolicySources.ReadFile("reference_policy_dependencies.json")
	if e != nil {
		return "", nil, nil, referencePolicyFailure("bundle_invalid")
	}
	var dependencies []ReferencePolicySource
	if json.Unmarshal(raw, &dependencies) != nil || len(dependencies) == 0 {
		return "", nil, nil, referencePolicyFailure("bundle_invalid")
	}
	id := referenceJSON(struct {
		Domain, Go, OS, Arch, Dependencies string
		Sources                            []ReferencePolicySource
	}{"hint-reference-code-bundle-v1", runtime.Version(), runtime.GOOS, runtime.GOARCH, referenceDigest(raw), sources})
	return id, sources, dependencies, nil
}
func resolveReferenceEntry(ref ReferencePreparationPolicyRef) (referencePolicyEntry, ReferencePreparationPolicyInfo, error) {
	return resolvePurposeEntry(ref, preparationPurpose)
}
func resolvePurposeEntry(ref ReferencePreparationPolicyRef, purpose string) (referencePolicyEntry, ReferencePreparationPolicyInfo, error) {
	var zero referencePolicyEntry
	var info ReferencePreparationPolicyInfo
	if !referencePolicyID.MatchString(ref.PolicyID) || !referencePolicySHA.MatchString(ref.PolicyDigest) {
		return zero, info, referencePolicyFailure("selector_invalid")
	}
	// Copy the exact installed value; no mutable nested policy or bounds escape.
	if len(referencePreparationPolicies) > 32 {
		return zero, info, referencePolicyFailure("catalogue_limit")
	}
	found := false
	for _, entry := range referencePreparationPolicies {
		if entry.ID == ref.PolicyID && entryPurpose(entry) == purpose {
			if found {
				return zero, info, referencePolicyFailure("catalogue_conflict")
			}
			zero = entry
			found = true
		}
	}
	if !found {
		return zero, info, referencePolicyFailure("policy_not_installed")
	}
	bundle, sources, deps, e := referenceBundleIdentity()
	if e != nil {
		return referencePolicyEntry{}, info, e
	}
	if zero.BundleDigest != bundle || zero.Procedure.identity != zero.ID || referenceEntryDigest(zero) != ref.PolicyDigest {
		return referencePolicyEntry{}, info, referencePolicyFailure("policy_identity_mismatch")
	}
	b := referenceBounds(zero.Procedure)
	if b.Operation <= 0 || b.Operation > 1800*time.Second || b.Resolve <= 0 || b.Resolve > 30*time.Second || b.Commit <= 0 || b.Commit > 30*time.Second || ((purpose == preparationPurpose) && (b.Build <= 0 || b.Build > 600*time.Second || b.Apply <= 0 || b.Apply > 300*time.Second)) || b.Overhead < 0 || b.Overhead > 10*time.Second || b.Cleanup <= 0 || b.Cleanup > 30*time.Second || b.ArchiveBytes <= 0 || b.ArchiveBytes > 96<<20 || b.TotalBytes < b.ArchiveBytes || b.TotalBytes > 256<<20 {
		return referencePolicyEntry{}, info, referencePolicyFailure("policy_bounds")
	}
	if purpose == frozenPurpose && (b.Resolve != 30*time.Second || b.Commit != 30*time.Second || b.Build != 0 || b.Apply != 0 || b.Overhead != 10*time.Second || b.Cleanup != 30*time.Second || b.ArchiveBytes != 96<<20 || b.TotalBytes != 256<<20 || !referencePolicySHA.MatchString(zero.Image)) {
		return referencePolicyEntry{}, info, referencePolicyFailure("policy_bounds")
	}
	if !filepath.IsAbs(zero.Endpoint) || filepath.Clean(zero.Endpoint) != zero.Endpoint || !referencePolicySHA.MatchString(zero.EngineRef) || zero.EngineVersion == "" || zero.EngineOS != "linux" || (zero.EngineArchitecture != "aarch64" && zero.EngineArchitecture != "arm64" && zero.EngineArchitecture != "x86_64" && zero.EngineArchitecture != "amd64") || zero.APIVersion != "1.56" {
		return referencePolicyEntry{}, info, referencePolicyFailure("policy_engine_binding")
	}
	catalogue, _ := referencePolicySources.ReadFile("reference_policy_catalogue.go")
	info = ReferencePreparationPolicyInfo{Version: ReferencePreparationPolicyVersion, Authority: "unknown", Rules: ReferencePolicyExecutionRules{IOInterface, referenceTARDomain, "none", "existing_task_image_build_policy", OracleRuns, 2e9, 2048 << 20, 1024}, Policy: ref, BundleDigest: bundle, CatalogueDigest: referenceJSON(struct {
		Source  string
		Entries []string
	}{referenceDigest(catalogue), referenceCatalogueDigests()}), EndpointDigest: referenceDigest([]byte(zero.Endpoint)), SocketIdentityDigest: referenceJSON(zero.Socket), EngineRef: zero.EngineRef, EngineVersion: zero.EngineVersion, EngineOS: zero.EngineOS, EngineArchitecture: zero.EngineArchitecture, APIVersion: zero.APIVersion, Bounds: b, Sources: append([]ReferencePolicySource{}, sources...), Dependencies: append([]ReferencePolicySource{}, deps...)}
	if purpose == frozenPurpose {
		info.Version = "hint-frozen-verify-policy-v1"
		info.Rules.BuildNetwork = "no_build"
	}
	return zero, info, nil
}
func referenceCatalogueDigests() []string {
	out := make([]string, len(referencePreparationPolicies))
	for i, p := range referencePreparationPolicies {
		out[i] = referenceEntryDigest(p)
	}
	return out
}

// ResolveReferencePreparationPolicy returns copied descriptions only. Supplying them
// back never enables execution; every entry point resolves the actual private catalogue.
func ResolveReferencePreparationPolicy(ref ReferencePreparationPolicyRef) (ReferencePreparationPolicyInfo, error) {
	_, info, e := resolveReferenceEntry(ref)
	return info, e
}
func referenceCheckContext(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return referencePolicyFailure("context_invalid")
	}
	return nil
}
func referenceCheckSocket(ctx context.Context, p referencePolicyEntry) error {
	pin, e := referenceSocketPinAt(ctx, p.Endpoint)
	if e != nil {
		return e
	}
	if pin != p.Socket {
		return referencePolicyFailure("socket_identity_mismatch")
	}
	return nil
}
func (d *Docker) referenceCheckEngine(ctx context.Context, p referencePolicyEntry) error {
	if d == nil || d.cli == nil {
		return referencePolicyFailure("engine_unbound")
	}
	info, e := d.cli.Info(ctx, client.InfoOptions{})
	if e != nil {
		return referencePolicyFailure("engine_unavailable")
	}
	x := info.Info
	if referenceDigest([]byte(x.ID)) != p.EngineRef || x.ServerVersion != p.EngineVersion || x.OSType != p.EngineOS || x.Architecture != p.EngineArchitecture {
		return referencePolicyFailure("engine_identity_mismatch")
	}
	return referenceCheckContext(ctx)
}

// OpenReferencePreparationEngine never consults Docker/proxy/TLS environment settings.
// Its original 10-second bound includes policy lookup, pins, dial, version and Info.
func OpenReferencePreparationEngine(parent context.Context, ref ReferencePreparationPolicyRef) (*Docker, error) {
	anchor := time.Now()
	if parent == nil {
		return nil, referencePolicyFailure("context_invalid")
	}
	ctx, cancel := context.WithDeadline(parent, anchor.Add(10*time.Second))
	defer cancel()
	if e := referenceCheckContext(ctx); e != nil {
		return nil, e
	}
	entry, info, e := resolveReferenceEntry(ref)
	if e != nil {
		return nil, e
	}
	if e = referenceCheckContext(ctx); e != nil {
		return nil, e
	}
	return openPinnedProcedureEngine(ctx, entry, info)
}

func openPinnedProcedureEngine(ctx context.Context, entry referencePolicyEntry, info ReferencePreparationPolicyInfo) (*Docker, error) {
	var e error
	if e = referenceCheckSocket(ctx, entry); e != nil {
		return nil, e
	}
	transport := &http.Transport{Proxy: nil}
	transport.DialContext = func(c context.Context, _, _ string) (net.Conn, error) {
		if e := referenceCheckSocket(c, entry); e != nil {
			return nil, e
		}
		conn, e := (&net.Dialer{}).DialContext(c, "unix", entry.Endpoint)
		if e != nil {
			return nil, referencePolicyFailure("engine_unavailable")
		}
		if e = referenceCheckSocket(c, entry); e != nil {
			_ = conn.Close()
			return nil, e
		}
		return conn, nil
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return referencePolicyFailure("redirect_refused") }}
	// WithHost's transport configuration precedes the final fixed dialer override.
	d, e := findDockerWithOptions(ctx, client.WithHTTPClient(httpClient), client.WithHost("unix://"+entry.Endpoint), client.WithDialContext(transport.DialContext), client.WithAPIVersion(entry.APIVersion))
	if e != nil {
		transport.CloseIdleConnections()
		return nil, referencePolicyFailure("engine_unavailable")
	}
	fail := func(e error) (*Docker, error) { d.Close(); return nil, e }
	if d.Version != entry.EngineVersion {
		return fail(referencePolicyFailure("engine_version_mismatch"))
	}
	if e = d.referenceCheckEngine(ctx, entry); e != nil {
		return fail(e)
	}
	if e = referenceCheckSocket(ctx, entry); e != nil {
		return fail(e)
	}
	if e = referenceCheckContext(ctx); e != nil {
		return fail(e)
	}
	d.referenceBinding = referenceBindingFor(entry, info)
	return d, nil
}

// PrepareReferenceForPolicy charges lookup and engine rechecks to the operation at
// ENTRY, then passes its earlier deadline into the one existing preparation core.
func (d *Docker) PrepareReferenceForPolicy(parent context.Context, taskDir string, req ReferencePolicyPrepareRequest) (ReferencePrepareAudit, error) {
	anchor := time.Now()
	a := ReferencePrepareAudit{State: "unknown", Authority: "unknown", Version: ReferencePrepareVersion}
	fail := func(code string) (ReferencePrepareAudit, error) {
		a.FailureCodes = []string{code}
		return a, referencePolicyFailure(code)
	}
	if parent == nil {
		return fail("context_invalid")
	}
	ctx, cancel := context.WithDeadline(parent, anchor.Add(1800*time.Second))
	defer cancel()
	if referenceCheckContext(ctx) != nil {
		return fail("context_invalid")
	}
	entry, info, e := resolveReferenceEntry(req.Policy)
	if e != nil {
		return a, e
	}
	// Tighten from the ENTRY anchor, not from completion of lookup or engine calls.
	ctx, tighten := context.WithDeadline(ctx, anchor.Add(entry.Procedure.operation))
	defer tighten()
	if referenceCheckContext(ctx) != nil {
		return fail("context_invalid")
	}
	binding := *referenceBindingFor(entry, info)
	if d == nil || d.referenceBinding == nil || *d.referenceBinding != binding {
		return fail("engine_unbound")
	}
	if e = referenceCheckSocket(ctx, entry); e != nil {
		return a, e
	}
	if e = d.referenceCheckEngine(ctx, entry); e != nil {
		return a, e
	}
	if referenceCheckContext(ctx) != nil {
		return fail("context_invalid")
	}
	a, e = d.PrepareReference(ctx, taskDir, ReferencePrepareOptions{OutputDir: req.OutputDir, Expected: req.Expected, policy: &entry.Procedure})
	a.InstalledPolicy = req.Policy
	a.PolicyBundleDigest = info.BundleDigest
	a.PolicyCatalogueDigest = info.CatalogueDigest
	a.PolicyEndpointDigest = info.EndpointDigest
	a.PolicySocketIdentityDigest = info.SocketIdentityDigest
	if e != nil {
		return a, referencePolicyFailure("preparation_unknown")
	}
	if referenceCheckContext(ctx) != nil {
		a.State = "unknown"
		a.FailureCodes = append(a.FailureCodes, "operation_late")
		return a, referencePolicyFailure("operation_late")
	}
	return a, nil
}

// The manifest pins reviewed transitive package/module bytes; source-to-installed-
// binary correspondence remains a separate deployment assumption, not this lookup.

func referenceBindingFor(e referencePolicyEntry, info ReferencePreparationPolicyInfo) *referenceEngineBinding {
	return &referenceEngineBinding{Purpose: entryPurpose(e), Policy: info.Policy, BundleDigest: info.BundleDigest, EndpointDigest: info.EndpointDigest, SocketDigest: info.SocketIdentityDigest, EngineRef: e.EngineRef, EngineVersion: e.EngineVersion, EngineOS: e.EngineOS, EngineArchitecture: e.EngineArchitecture, APIVersion: e.APIVersion}
}
