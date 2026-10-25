//go:build unix

package runner

// Owned synthetic Unix sockets and existing fake engine handler only. No real engine.
import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type referenceCountListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *referenceCountListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e == nil {
		l.accepted.Add(1)
	}
	return c, e
}
func policyUnixFixture(t *testing.T, mode string) (referencePolicyEntry, *referenceFake, *referenceCountListener) {
	t.Helper()
	dir, e := os.MkdirTemp("/tmp", "hint-rp-")
	if e != nil {
		t.Fatal(e)
	}
	dir, e = filepath.EvalSymlinks(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "engine.sock")
	listener, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	counted := &referenceCountListener{Listener: listener}
	d, f := referenceEngineListener(t, mode, counted)
	t.Cleanup(func() { _ = d.Close() })
	_, opts := referenceTaskOptions(t)
	pin, e := referenceSocketPinAt(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	bundle, _, _, e := referenceBundleIdentity()
	if e != nil {
		t.Fatal(e)
	}
	entry := referencePolicyEntry{ID: "synthetic.reference-v1", BundleDigest: bundle, Endpoint: path, Socket: pin, EngineRef: referenceDigest([]byte("synthetic-engine")), EngineVersion: "synthetic-1", EngineOS: "linux", EngineArchitecture: "aarch64", APIVersion: "1.56", Procedure: *opts.policy}
	entry.Procedure.identity = entry.ID
	return entry, f, counted
}
func installReferenceTestEntry(t *testing.T, p referencePolicyEntry) ReferencePreparationPolicyRef {
	t.Helper()
	saved := referencePreparationPolicies
	referencePreparationPolicies = []referencePolicyEntry{p}
	t.Cleanup(func() { referencePreparationPolicies = saved })
	return ReferencePreparationPolicyRef{p.ID, referenceEntryDigest(p)}
}
func TestReferencePolicyUnixPositiveAndCopiedInfo(t *testing.T) {
	entry, f, _ := policyUnixFixture(t, "")
	ref := installReferenceTestEntry(t, entry)
	one, e := ResolveReferencePreparationPolicy(ref)
	if e != nil {
		t.Fatal(e)
	}
	one.Bounds.Operation = 0
	one.Dependencies[0].Digest = "mutated"
	one.Sources[0].Digest = "mutated"
	one.Policy.PolicyDigest = "mutated"
	two, e := ResolveReferencePreparationPolicy(ref)
	if e != nil || two.Bounds.Operation != entry.Procedure.operation || two.Dependencies[0].Digest == "mutated" || two.Sources[0].Digest == "mutated" || two.Policy != ref {
		t.Fatal("info alias", two, e)
	}
	// Poisoned values are synthetic; this path must not consult them or contact them.
	envEntry, envFake, envListener := policyUnixFixture(t, "")
	envFake.mu.Lock()
	envFake.infoID = "environment-engine"
	envFake.mu.Unlock()
	t.Setenv("DOCKER_HOST", "unix://"+envEntry.Endpoint)
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/no-synthetic-cert")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	d, e := OpenReferencePreparationEngine(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, opts := referenceTaskOptions(t)
	a, e := d.PrepareReferenceForPolicy(context.Background(), task, ReferencePolicyPrepareRequest{ref, opts.OutputDir, opts.Expected})
	if e != nil || a.State != "prepared_reference_observed" || a.Authority != "unknown" {
		t.Fatal(a, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if envListener.accepted.Load() != 0 {
		t.Fatal("policy consulted environment endpoint")
	}
	if a.InstalledPolicy != ref || a.PolicyBundleDigest != two.BundleDigest || a.PolicyCatalogueDigest != two.CatalogueDigest || a.PolicyDigest == ref.PolicyDigest {
		t.Fatal("installed/procedure policy domains conflated", a)
	}
	if f.builds != 1 || f.creates != 5 {
		t.Fatal("existing preparation path changed", f.builds, f.creates)
	}
}
func TestReferencePolicyDefaultMakesNoUnixConnection(t *testing.T) {
	entry, _, listener := policyUnixFixture(t, "")
	t.Setenv("DOCKER_HOST", "unix://"+entry.Endpoint)
	if _, e := OpenReferencePreparationEngine(context.Background(), ReferencePreparationPolicyRef{entry.ID, referenceEntryDigest(entry)}); e == nil {
		t.Fatal("uninstalled entry dialled")
	}
	if listener.accepted.Load() != 0 {
		t.Fatal("default contacted environment engine")
	}
}
func TestReferencePolicyUnixRejectsPinsAndEngine(t *testing.T) {
	for _, mode := range []string{"bundle", "socket", "engine", "version", "arch", "selector", "alias"} {
		t.Run(mode, func(t *testing.T) {
			entry, f, listener := policyUnixFixture(t, "")
			switch mode {
			case "bundle":
				entry.BundleDigest = "sha256:" + strings.Repeat("f", 64)
			case "socket":
				entry.Socket.Inode++
			case "engine":
				entry.EngineRef = referenceDigest([]byte("other"))
			case "version":
				entry.EngineVersion = "other-version"
			case "arch":
				entry.EngineArchitecture = "amd64"
			case "alias":
				p := entry.Endpoint + "-alias"
				if e := os.Symlink(entry.Endpoint, p); e != nil {
					t.Fatal(e)
				}
				entry.Endpoint = p
			}
			ref := installReferenceTestEntry(t, entry)
			if mode == "selector" {
				ref.PolicyDigest = "sha256:" + strings.Repeat("a", 64)
			}
			d, e := OpenReferencePreparationEngine(context.Background(), ref)
			if e == nil {
				d.Close()
				t.Fatal("mismatch accepted")
			}
			if !errors.Is(e, ErrReferencePreparationPolicy) || strings.Contains(e.Error(), entry.Endpoint) || strings.Contains(e.Error(), "other-version") {
				t.Fatal("raw error leaked", e)
			}
			if mode == "bundle" || mode == "socket" || mode == "selector" || mode == "alias" {
				if listener.accepted.Load() != 0 {
					t.Fatal("preflight mismatch dialled")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.builds != 0 || f.creates != 0 {
				t.Fatal("preflight executed work")
			}
		})
	}
}
func TestReferencePolicyPrepareRechecksBindingAndEngine(t *testing.T) {
	entry, f, _ := policyUnixFixture(t, "")
	ref := installReferenceTestEntry(t, entry)
	d, e := OpenReferencePreparationEngine(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, opts := referenceTaskOptions(t)
	f.mu.Lock()
	f.infoID = "changed-engine"
	f.mu.Unlock()
	a, e := d.PrepareReferenceForPolicy(context.Background(), task, ReferencePolicyPrepareRequest{ref, opts.OutputDir, opts.Expected})
	if e == nil || a.State != "unknown" {
		t.Fatal(a, e)
	}
	if _, e = os.Stat(opts.OutputDir); !os.IsNotExist(e) {
		t.Fatal("output created before failed recheck")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.builds != 0 || f.creates != 0 {
		t.Fatal("changed engine executed")
	}
}
func TestReferencePolicyWrapperBudgetIncludesEngineRecheck(t *testing.T) {
	entry, f, _ := policyUnixFixture(t, "info_slow")
	entry.Procedure.operation = 2500 * time.Millisecond
	entry.Procedure.resolve = time.Millisecond
	entry.Procedure.commit = time.Millisecond
	entry.Procedure.build = time.Millisecond
	entry.Procedure.apply = time.Millisecond
	entry.Procedure.overhead = 0
	entry.Procedure.cleanup = time.Millisecond
	ref := installReferenceTestEntry(t, entry)
	d, e := OpenReferencePreparationEngine(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	// One genuine declared case needs two seconds plus fixed slots. Recheck takes600ms.
	task, opts := referenceTaskOptions(t)
	spec := `{"interface":"cli-file-v1","entry":["sh","tool.sh"],"cases":["lower"],"case_seconds":1,"compare":"exact"}`
	if e = os.WriteFile(filepath.Join(task, "tests", "io", "spec.json"), []byte(spec), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(task, "tests", "io", "inputs", "mixed.in")); e != nil {
		t.Fatal(e)
	}
	id, e := InspectReferencePreparationInputs(context.Background(), task)
	if e != nil {
		t.Fatal(e)
	}
	a, e := d.PrepareReferenceForPolicy(context.Background(), task, ReferencePolicyPrepareRequest{ref, opts.OutputDir, id})
	if e == nil || a.State != "unknown" {
		t.Fatal("recheck got fresh operation budget", a, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.builds != 0 || f.creates != 0 {
		t.Fatal("capacity counted after recheck as fresh", f.builds, f.creates)
	}
}
func TestReferencePolicyLegacyFindDockerStillUsesEnvironment(t *testing.T) {
	entry, _, _ := policyUnixFixture(t, "")
	t.Setenv("DOCKER_HOST", "unix://"+entry.Endpoint)
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_API_VERSION", "1.56")
	d, e := FindDocker(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if d.Version != "synthetic-1" || d.referenceBinding != nil {
		t.Fatal("legacy acquired a policy binding")
	}
	task, opts := referenceTaskOptions(t)
	ref := installReferenceTestEntry(t, entry)
	if _, e = d.PrepareReferenceForPolicy(context.Background(), task, ReferencePolicyPrepareRequest{ref, opts.OutputDir, opts.Expected}); e == nil {
		t.Fatal("legacy env client became policy-bound")
	}
}

func TestReferencePolicyOpenUsesEarlierCallerBudget(t *testing.T) {
	entry, f, _ := policyUnixFixture(t, "info_slow")
	ref := installReferenceTestEntry(t, entry)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	d, e := OpenReferencePreparationEngine(ctx, ref)
	if e == nil {
		d.Close()
		t.Fatal("late constructor accepted")
	}
	if time.Since(start) > time.Second || !errors.Is(e, ErrReferencePreparationPolicy) {
		t.Fatal("constructor renewed budget", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.builds != 0 || f.creates != 0 {
		t.Fatal("constructor executed work")
	}
}
func TestReferencePolicyDuplicateCatalogueIsNotASelector(t *testing.T) {
	entry, _, listener := policyUnixFixture(t, "")
	ref := installReferenceTestEntry(t, entry)
	referencePreparationPolicies = append(referencePreparationPolicies, entry)
	if _, e := OpenReferencePreparationEngine(context.Background(), ref); e == nil || listener.accepted.Load() != 0 {
		t.Fatal("duplicate policy dialled", e)
	}
}
func TestReferencePolicyPostLookupSocketReplacementRefuses(t *testing.T) {
	entry, _, _ := policyUnixFixture(t, "")
	ref := installReferenceTestEntry(t, entry)
	d, e := OpenReferencePreparationEngine(context.Background(), ref)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	// Replace the bound socket path with a regular synthetic file, without touching an engine.
	if e = os.Remove(entry.Endpoint); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(entry.Endpoint, []byte("synthetic"), 0600); e != nil {
		t.Fatal(e)
	}
	task, opts := referenceTaskOptions(t)
	a, e := d.PrepareReferenceForPolicy(context.Background(), task, ReferencePolicyPrepareRequest{ref, opts.OutputDir, opts.Expected})
	if e == nil || a.State != "unknown" {
		t.Fatal("replaced socket accepted", a, e)
	}
}
