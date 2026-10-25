//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	githint "github.com/neprel/git-hint"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
	"golang.org/x/sys/unix"
)

func captureFixture(t *testing.T) (definitionFixture, WorkDefinitionRef) {
	t.Helper()
	cwd, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	cwd, e = filepath.EvalSymlinks(cwd)
	if e != nil {
		t.Fatal(e)
	}
	domain, e := os.MkdirTemp(cwd, ".capture-source-fixture-")
	if e != nil {
		t.Fatal(e)
	}
	owned, e := os.Lstat(domain)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		info, e := os.Lstat(domain)
		if e != nil || !os.SameFile(owned, info) || !info.IsDir() {
			t.Error("owned capture fixture replaced")
			return
		}
		if e = os.RemoveAll(domain); e != nil {
			t.Error(e)
		}
	})
	for _, n := range []string{"root", "home", "xdg", "keys"} {
		if e = os.Mkdir(filepath.Join(domain, n), 0700); e != nil {
			t.Fatal(e)
		}
	}
	git, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	git, e = filepath.EvalSymlinks(git)
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range os.Environ() {
		k, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
			old := os.Getenv(k)
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, old) })
		}
	}
	t.Setenv("PATH", filepath.Dir(git)+":/usr/bin:/bin")
	t.Setenv("HOME", filepath.Join(domain, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(domain, "xdg"))
	t.Setenv("HINT_KEY_DIR", filepath.Join(domain, "keys"))
	root := filepath.Join(domain, "root")
	testkit.Git(t, root, "init", "-q")
	testkit.WriteFile(t, root, "main.go", "package main\nfunc Value() int {return 1}\n")
	testkit.Git(t, root, "add", "main.go")
	testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "-qm", "synthetic source")
	f := definitionFixtureAt(t, root)
	return f, writeDefinition(t, f)
}
func captureReq(f definitionFixture, ref WorkDefinitionRef) CaptureSourceRequest {
	return CaptureSourceRequest{Label: f.request.Label, Definition: ref}
}
func TestCaptureSourceActualCreateReloadAndNoAuthority(t *testing.T) {
	f, def := captureFixture(t)
	ctx := context.Background()
	a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, def), f.now)
	if e != nil {
		t.Fatal("genuine installed Git/source CREATE refused", e)
	}
	if a == nil || a.Source() == nil || a.Acceptance() == nil {
		t.Fatal("success lacks paired result/witness")
	}
	loaded, e := LoadCaptureSource(ctx, f.s, *a.Source())
	if e != nil {
		t.Fatal(e)
	}
	v := loaded.Identity()
	if v.Authority != "unknown" || v.State != "observed_selected_source_consistent" || v.Basis != "repeated_local_selected_artifact_observation" || v.DefinitionRef != def || !captureAcceptanceMatches(a.Acceptance(), *a.Source(), v) {
		t.Fatal("wrong factual/witness binding")
	}
	if !captureBracketsValid(v.Brackets, v.CommitPrepareNS) || v.ClockScope != v.RecordRef || v.OuterStats.Dispatches < 1 {
		t.Fatal("wrong private clock/prefix counts")
	}
	for _, o := range v.Objects {
		raw, ok := loaded.Object(o.Digest)
		if !ok || library.Digest(raw) != o.Digest || int64(len(raw)) != o.Bytes {
			t.Fatal("object equality")
		}
		if len(raw) > 0 {
			raw[0] ^= 1
			again, _ := loaded.Object(o.Digest)
			if bytes.Equal(raw, again) {
				t.Fatal("object alias")
			}
		}
	}
	v.Objects[0].Roles[0] = "changed"
	if loaded.Identity().Objects[0].Roles[0] == "changed" {
		t.Fatal("identity alias")
	}
	ref := a.Source()
	ref.RecordRef = "changed"
	if a.Source().RecordRef == "changed" {
		t.Fatal("ref alias")
	}
	witness := a.Acceptance()
	if _, e = json.Marshal(witness); e == nil {
		t.Fatal("serialized authority")
	}
	if json.Unmarshal([]byte(`{}`), new(CaptureSourceAcceptance)) == nil {
		t.Fatal("deserialized authority")
	}
	forged := *witness
	forged.checkpoint = preworkAcceptance{}
	if captureAcceptanceMatches(&forged, *a.Source(), loaded.Identity()) {
		t.Fatal("forged issuer")
	}
	forged = *witness
	forged.checkpoint = mintPreworkAcceptance(preworkReferencePreparation)
	if captureAcceptanceMatches(&forged, *a.Source(), loaded.Identity()) {
		t.Fatal("wrong purpose")
	}
	forged = *witness
	forged.end = time.Now().Add(-time.Second)
	if captureAcceptanceMatches(&forged, *a.Source(), loaded.Identity()) {
		t.Fatal("renewed deadline")
	}
	if again, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, def), f.now); e == nil || again != nil {
		t.Fatal("CREATE replay minted authority")
	}
	// Factual reload remains factual after later received history changes.
	if _, e := WorkStart(f.s, f.request.Label, nil, "", "family", nil, f.now); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadCaptureSource(ctx, f.s, *a.Source()); e != nil {
		t.Fatal("loader recertifies current history", e)
	}
}

type captureTestObserver func(string)

func (f captureTestObserver) captureSourceBoundary(p string) { f(p) }
func captureObserved(ctx context.Context, f func(string)) context.Context {
	return context.WithValue(ctx, captureSourceObserverKey{}, captureTestObserver(f))
}
func TestCaptureSourceRefusesMutationsAndLeavesPending(t *testing.T) {
	for _, tc := range []struct {
		name, phase string
		mutate      func(*testing.T, definitionFixture)
	}{
		{"source", "capture_a_end", func(t *testing.T, f definitionFixture) { testkit.WriteFile(t, f.s.Root, "main.go", "changed source") }},
		{"index", "capture_a_end", func(t *testing.T, f definitionFixture) {
			testkit.WriteFile(t, f.s.Root, "main.go", "changed index")
			testkit.Git(t, f.s.Root, "add", "main.go")
		}},
		{"ignore", "capture_a_end", func(t *testing.T, f definitionFixture) { testkit.WriteFile(t, f.s.Root, ".gitignore", "main.go\n") }},
		{"config", "capture_a_end", func(t *testing.T, f definitionFixture) { testkit.Git(t, f.s.Root, "config", "core.filemode", "false") }},
		{"stored_pointer", "capture_b_end", func(t *testing.T, f definitionFixture) {
			p := f.s.Path("state", "exports", "state.json")
			raw, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(p, append(raw, ' '), 0600)
		}},
		{"store_config", "capture_b_end", func(t *testing.T, f definitionFixture) {
			f.s.Cfg.ServerURL = "https://synthetic.invalid"
			if e := f.s.SaveConfig(); e != nil {
				t.Fatal(e)
			}
		}},
		{"history", "capture_b_end", func(t *testing.T, f definitionFixture) {
			p := f.s.Path("state", "work")
			os.MkdirAll(p, 0700)
			os.WriteFile(filepath.Join(p, "bad.json"), []byte(`{}`), 0600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, ref := captureFixture(t)
			once := false
			ctx := captureObserved(context.Background(), func(p string) {
				if p == tc.phase && !once {
					once = true
					tc.mutate(t, f)
				}
			})
			a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, ref), f.now)
			if e == nil || a != nil || !once {
				t.Fatal("mutation accepted", e)
			}
			if _, e := f.s.ReadStateDirBounded(context.Background(), 1024, "state", library.CaptureReservationStateDir); e != nil {
				t.Fatal("pending evidence lost", e)
			}
			if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, ref), f.now); e == nil || a != nil {
				t.Fatal("pending resumed")
			}
		})
	}
}
func TestCaptureSourceCancellationAndLateAuditNoWitness(t *testing.T) {
	for _, phase := range []string{"policy_commit_end", "reservation_commit_end", "view_open_end", "capture_a_end", "capture_b_end", "view_close_end", "objects_and_receipts_end", "audit_commit_end", "final_reload_end", "unlock_end"} {
		t.Run(phase, func(t *testing.T) {
			f, ref := captureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = captureObserved(ctx, func(p string) {
				if p == phase {
					cancel()
				}
			})
			a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, ref), f.now)
			if e == nil || a != nil || !errors.Is(e, context.Canceled) {
				t.Fatal("cancel minted result", a, e)
			}
			if phase == "audit_commit_end" || phase == "final_reload_end" || phase == "unlock_end" {
				files, e := f.s.ReadStateDirBounded(context.Background(), 2, "state", library.CaptureSourceStateDir)
				if e != nil || len(files) != 1 {
					t.Fatal("durable audit lost")
				}
				raw, e := f.s.ReadStateBounded(context.Background(), captureRecordMax, "state", library.CaptureSourceStateDir, files[0].Name())
				if e != nil {
					t.Fatal(e)
				}
				auditRef := CaptureSourceRef{strings.TrimSuffix(files[0].Name(), ".json"), library.Digest(raw)}
				if _, e := LoadCaptureSource(context.Background(), f.s, auditRef); e != nil {
					t.Fatal("late factual audit not loadable", e)
				}
			}
		})
	}
}
func TestCaptureSourceSourceSetAndEveryDependencyHashed(t *testing.T) {
	names := []string{}
	entries, e := os.ReadDir(".")
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if captureSourceFamily(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	embedded, _ := fs.ReadDir(captureSources, ".")
	got := []string{}
	for _, v := range embedded {
		got = append(got, v.Name())
	}
	sort.Strings(got)
	if !reflect.DeepEqual(names, got) {
		t.Fatalf("source family differs %v %v", names, got)
	}
	deps := []string{}
	fs.WalkDir(githint.CaptureSourceAssets, ".", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			t.Fatal(e)
		}
		if !d.IsDir() {
			deps = append(deps, path)
		}
		return nil
	})
	expectedDeps := []string{"capture_source_assets.go", "go.mod", "go.sum", "internal/project/state.go", "internal/project/collect.go"}
	for _, dir := range []string{"internal/evals/trees", "internal/readgit", "internal/evals/library", "internal/store", "internal/hint", "internal/scan", "internal/snapshot"} {
		entries, e := os.ReadDir(filepath.Join("..", "..", dir))
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				p := dir + "/" + entry.Name()
				expectedDeps = append(expectedDeps, p)
				found := false
				for _, n := range deps {
					found = found || n == p
				}
				if !found {
					t.Fatal("dependency missing", p)
				}
			}
		}
	}
	for _, family := range []string{"v0.1", "v0.2", "v0.3"} {
		entries, e := os.ReadDir(filepath.Join("..", "..", "schemas", family))
		if e != nil {
			t.Fatal(e)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".schema.json") {
				expectedDeps = append(expectedDeps, "schemas/"+family+"/"+entry.Name())
			}
		}
	}
	sort.Strings(expectedDeps)
	sort.Strings(deps)
	if !reflect.DeepEqual(expectedDeps, deps) {
		t.Fatal("declared dependency family differs from compiled assets", expectedDeps, deps)
	}
	original := captureSourceIdentity()
	for _, which := range []struct {
		local  bool
		source fs.FS
	}{{true, captureSources}, {false, githint.CaptureSourceAssets}} {
		fs.WalkDir(which.source, ".", func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				t.Fatal(e)
			}
			if d.IsDir() {
				return nil
			}
			replacement := fstest.MapFS{}
			fs.WalkDir(which.source, ".", func(p string, n fs.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if !n.IsDir() {
					raw, _ := fs.ReadFile(which.source, p)
					if p == path {
						raw = append(raw, ' ')
					}
					replacement[p] = &fstest.MapFile{Data: raw}
				}
				return nil
			})
			var changed string
			if which.local {
				changed = captureSourceIdentityFrom(replacement, githint.CaptureSourceAssets)
			} else {
				changed = captureSourceIdentityFrom(captureSources, replacement)
			}
			if changed == original || changed == "" {
				t.Fatal("unhashed dependency", path)
			}
			return nil
		})
	}
}

func TestCaptureSourceAdmissionLimitsAndBusy(t *testing.T) {
	f, ref := captureFixture(t)
	req := captureReq(f, ref)
	for _, bad := range []CaptureSourceRequest{{Label: "wrong", Definition: ref}, {Label: req.Label, Definition: WorkDefinitionRef{ref.RecordRef, "sha256:" + strings.Repeat("0", 64)}}, {Label: req.Label, Definition: ref, Limits: trees.Limits{MaxFiles: 10001}}} {
		if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, bad, f.now); e == nil || a != nil {
			t.Fatal("invalid request admitted")
		}
	}
	lock, e := f.s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	a, e := CaptureWorkDefinitionSource(context.Background(), f.s, req, f.now)
	if a != nil || !errors.Is(e, store.ErrBusy) {
		t.Fatal("nested lock admitted", e)
	}
	if e = lock.UnlockChecked(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a, e := CaptureWorkDefinitionSource(ctx, f.s, req, f.now); e == nil || a != nil {
		t.Fatal("cancelled request admitted")
	}
	if _, e := os.Stat(f.s.Path("state", library.CapturePolicyStateDir)); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("refused early request allocated operation")
	}
	req.Limits = trees.Limits{MaxFileBytes: 1024, MaxTotalBytes: 2048, MaxFiles: 16}
	a, e = CaptureWorkDefinitionSource(context.Background(), f.s, req, f.now)
	if e != nil || a.Source() == nil {
		t.Fatal("tighter valid profile refused", e)
	}
}
func TestCaptureSourceLoaderRejectsRecordAndObjectMutations(t *testing.T) {
	f, ref := captureFixture(t)
	a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, ref), f.now)
	if e != nil {
		t.Fatal(e)
	}
	h, e := LoadCaptureSource(context.Background(), f.s, *a.Source())
	if e != nil {
		t.Fatal(e)
	}
	v := h.Identity()
	path := f.s.Path("state", library.CaptureSourceStateDir, v.RecordRef+".json")
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	mutations := []func(*CaptureSourceIdentity){func(v *CaptureSourceIdentity) { v.Authority = "trusted" }, func(v *CaptureSourceIdentity) { v.RootBinding = hint.EvalRef(f.s.Key(), "wrong", "root") }, func(v *CaptureSourceIdentity) { v.OuterInvocation = "sha256:" + strings.Repeat("0", 64) }, func(v *CaptureSourceIdentity) { v.Definition.PlanBinding.PlanID = strings.Repeat("0", 32) }, func(v *CaptureSourceIdentity) { v.ReceiptObjects[0].Bytes++ }, func(v *CaptureSourceIdentity) { v.Objects[0].Bytes++ }, func(v *CaptureSourceIdentity) { v.Brackets[0].BeginNS = -1 }, func(v *CaptureSourceIdentity) { v.CommitPrepareNS = int64(30 * time.Second) }, func(v *CaptureSourceIdentity) { v.Writer = "sha256:" + strings.Repeat("0", 64) }}
	for i, mutate := range mutations {
		var changed CaptureSourceIdentity
		if json.Unmarshal(original, &changed) != nil {
			t.Fatal("fixture")
		}
		mutate(&changed)
		raw, e := hint.Canonical(changed)
		if e != nil {
			t.Fatal(e)
		}
		os.WriteFile(path, raw, 0600)
		if h, e := LoadCaptureSource(context.Background(), f.s, CaptureSourceRef{v.RecordRef, library.Digest(raw)}); e == nil || h != nil {
			t.Fatal("record mutation accepted", i)
		}
	}
	for _, raw := range [][]byte{bytes.Replace(original, []byte(`"authority":"unknown"`), []byte(`"authority":null`), 1), bytes.Replace(original, []byte(`"authority":"unknown"`), []byte(`"authority":"unknown","authority":"unknown"`), 1), append(append([]byte(nil), original...), []byte(`{}`)...)} {
		os.WriteFile(path, raw, 0600)
		if h, e := LoadCaptureSource(context.Background(), f.s, CaptureSourceRef{v.RecordRef, library.Digest(raw)}); e == nil || h != nil {
			t.Fatal("malformed record accepted")
		}
	}
	os.WriteFile(path, original, 0600)
	// The normal object location helper, not a second reader, selects the exact retained object.
	target := ""
	for _, o := range v.Objects {
		for _, role := range o.Roles {
			if role == "selected_content" {
				target = o.Digest
			}
		}
	}
	if target == "" {
		t.Fatal("no genuine content object")
	}
	hsh := strings.TrimPrefix(target, "sha256:")
	found := f.s.Path("state", "evals", "objects", hsh[:2], hsh[2:])
	raw, _ := os.ReadFile(found)
	os.WriteFile(found, []byte("damaged"), 0600)
	if h, e := LoadCaptureSource(context.Background(), f.s, *a.Source()); e == nil || h != nil {
		t.Fatal("damaged raw object accepted")
	}
	os.WriteFile(found, raw, 0600)
}

func TestCaptureSourceDurableAuditAfterOriginalDeadlineHasNoWitness(t *testing.T) {
	f, ref := captureFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reached := false
	ctx = captureObserved(ctx, func(phase string) {
		if phase == "audit_commit_end" {
			reached = true
			<-ctx.Done()
		}
	})
	a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, ref), f.now)
	if !reached || a != nil || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("original endpoint not enforced after durable audit", reached, a, e)
	}
	files, e := f.s.ReadStateDirBounded(context.Background(), 2, "state", library.CaptureSourceStateDir)
	if e != nil || len(files) != 1 {
		t.Fatal(e)
	}
	raw, e := f.s.ReadStateBounded(context.Background(), captureRecordMax, "state", library.CaptureSourceStateDir, files[0].Name())
	if e != nil {
		t.Fatal(e)
	}
	if h, e := LoadCaptureSource(context.Background(), f.s, CaptureSourceRef{strings.TrimSuffix(files[0].Name(), ".json"), library.Digest(raw)}); e != nil || h == nil {
		t.Fatal("late bytes not factual", e)
	}
}

func TestCaptureSourceActualUnlockFailureNeverMints(t *testing.T) {
	f, ref := captureFixture(t)
	closed := false
	ctx := captureObserved(context.Background(), func(phase string) {
		if phase != "unlock_begin" {
			return
		}
		var named unix.Stat_t
		if e := unix.Stat(f.s.Path("state", "lock"), &named); e != nil {
			t.Fatal(e)
		}
		// Close only this producer's known, still-owned descriptor. No foreign FD is touched.
		for fd := 3; fd < 1024; fd++ {
			var st unix.Stat_t
			if unix.Fstat(fd, &st) == nil && st.Dev == named.Dev && st.Ino == named.Ino {
				if closed {
					t.Fatal("ambiguous owned lock descriptor")
				}
				if e := unix.Close(fd); e != nil {
					t.Fatal(e)
				}
				closed = true
				break
			}
		}
		if !closed {
			t.Fatal("owned lock descriptor unavailable")
		}
	})
	a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, ref), f.now)
	if !closed || a != nil || e == nil || !strings.Contains(e.Error(), "unlock_failed") {
		t.Fatal("release error minted witness", closed, a, e)
	}
	lock, e := f.s.TryLockStateContext(context.Background())
	if e != nil {
		t.Fatal("ownership leaked", e)
	}
	if e = lock.UnlockChecked(); e != nil {
		t.Fatal(e)
	}
}

func TestCaptureSourceFailedDefinitionDoesNotWedgeDifferentDefinition(t *testing.T) {
	f, first := captureFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	ctx = captureObserved(ctx, func(phase string) {
		if phase == "capture_a_end" {
			reached = true
			cancel()
		}
	})
	if a, e := CaptureWorkDefinitionSource(ctx, f.s, captureReq(f, first), f.now); a != nil || !errors.Is(e, context.Canceled) || !reached {
		t.Fatal("first genuine partial capture did not fail as intended", a, e, reached)
	}
	sources, e := f.s.ReadStateDirBounded(context.Background(), 8, "state", library.CaptureSourceStateDir)
	if e != nil || len(sources) != 0 {
		t.Fatal("expected empty audit directory", sources, e)
	}
	retained := map[string][]byte{}
	for _, dir := range []string{library.CapturePolicyStateDir, library.CaptureReservationStateDir} {
		files, e := f.s.ReadStateDirBounded(context.Background(), 8, "state", dir)
		if e != nil || len(files) != 1 {
			t.Fatal("failed operation records missing", dir, e)
		}
		path := f.s.Path("state", dir, files[0].Name())
		raw, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		retained[path] = raw
	}
	otherRequest := f.request
	otherRequest.Label = "synthetic-different-unit"
	second, e := WriteWorkDefinitionInputs(context.Background(), f.s, otherRequest, f.now)
	if e != nil {
		t.Fatal("genuine second definition refused", e)
	}
	request := CaptureSourceRequest{Label: otherRequest.Label, Definition: second}
	created, e := CaptureWorkDefinitionSource(context.Background(), f.s, request, f.now)
	if e != nil || created == nil || created.Source() == nil || created.Acceptance() == nil {
		t.Fatal("different definition wedged by empty source directory", e)
	}
	if a, e := CaptureWorkDefinitionSource(context.Background(), f.s, captureReq(f, first), f.now); a != nil || e == nil {
		t.Fatal("same failed definition resumed")
	}
	for path, before := range retained {
		after, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(before, after) {
			t.Fatal("failed evidence changed", e)
		}
	}
	if _, e := f.l.CollectGarbage(); !errors.Is(e, library.ErrDefinitionReferences) {
		t.Fatal("retention barrier weakened", e)
	}
}
