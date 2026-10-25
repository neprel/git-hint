//go:build unix

package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/runner"
	"github.com/neprel/git-hint/internal/hint"
	"github.com/neprel/git-hint/internal/testkit/referenceengine"
	"golang.org/x/sys/unix"
)

var referenceFixture = flag.String("reference-fixture", "", "owned synthetic test-build selector only")

type referenceSelector struct {
	Policy       runner.ReferencePreparationPolicyRef
	CatalogueSHA string
	ModeFile     string
}

func TestProjectReferenceOverlayChild(t *testing.T) {
	if *referenceFixture == "" {
		t.Skip("test-build overlay only")
	}
	raw, e := os.ReadFile(*referenceFixture)
	if e != nil {
		t.Fatal(e)
	}
	var sel referenceSelector
	if json.Unmarshal(raw, &sel) != nil {
		t.Fatal("selector")
	}
	info, e := runner.ResolveReferencePreparationPolicy(sel.Policy)
	if e != nil {
		t.Fatal(e)
	}
	if info.Authority != "unknown" {
		t.Fatal(info)
	}
	t.Run("real-persistence-replay-load", func(t *testing.T) {
		f := definitionFixtureNew(t)
		ref := writeDefinition(t, f)
		req := PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy}
		a, e := PrepareFinalReference(context.Background(), f.s, req, f.now)
		if e != nil {
			t.Fatalf("prepare: %+v %v", a, e)
		}
		if a.Prepared() == nil || a.Outcome() == nil {
			t.Fatal("no persisted prepared")
		}
		h, e := LoadPreparedFinalReference(context.Background(), f.s, *a.Prepared())
		if e != nil {
			t.Fatal(e)
		}
		id := h.Identity()
		if id.Authority != "unknown" || id.DefinitionRef != ref || id.Audit.FinalTAR.Digest == id.Definition.QualifiedInputIdentity.ReferencePatchArchive {
			t.Fatal("domain conflation", id)
		}
		bytes, ok := h.Object(id.Audit.FinalTAR.Digest)
		if !ok || library.Digest(bytes) != id.Audit.FinalTAR.Digest {
			t.Fatal("TAR")
		}
		bytes[0] ^= 1
		again, _ := h.Object(id.Audit.FinalTAR.Digest)
		if reflect.DeepEqual(bytes, again) {
			t.Fatal("mutable bytes")
		}
		id.Artifacts[0].Digest = "changed"
		if h.Identity().Artifacts[0].Digest == "changed" {
			t.Fatal("mutable identity")
		}
		before, _ := os.ReadFile(f.s.Path("state", library.ReferencePreparationStateDir, h.identity.Definition.UnitRef, strings.Split(a.Prepared().RecordRef, ".")[1], "outcome.json"))
		replay, e := PrepareFinalReference(context.Background(), f.s, req, f.now.Add(time.Hour))
		if e != nil || *replay.Prepared() != *a.Prepared() {
			t.Fatal("replay", e)
		}
		after, _ := os.ReadFile(f.s.Path("state", library.ReferencePreparationStateDir, h.identity.Definition.UnitRef, strings.Split(a.Prepared().RecordRef, ".")[1], "outcome.json"))
		if !reflect.DeepEqual(before, after) {
			t.Fatal("rewritten terminal")
		}
		bad := req
		bad.Definition.RecordDigest = "sha256:" + strings.Repeat("a", 64)
		if _, e = PrepareFinalReference(context.Background(), f.s, bad, f.now); e == nil {
			t.Fatal("changed definition replay")
		}
		bad = req
		bad.Label = "other-unit"
		if _, e = PrepareFinalReference(context.Background(), f.s, bad, f.now); e == nil {
			t.Fatal("wrong unit")
		}

		parts := strings.Split(a.Prepared().RecordRef, ".")
		base := []string{"state", library.ReferencePreparationStateDir, parts[0], parts[1]}
		output := f.s.Path(append(base, "output")...)
		for _, kind := range []string{"extra", "missing", "changed", "symlink", "hardlink", "size-audit", "digest-audit", "duplicate-case"} {
			t.Run(kind, func(t *testing.T) {
				audit := h.Identity().Audit
				tarPath := filepath.Join(output, "final.tar")
				saved, e := os.ReadFile(tarPath)
				if e != nil {
					t.Fatal(e)
				}
				restore := func() {
					_ = os.Remove(tarPath)
					_ = os.WriteFile(tarPath, saved, 0600)
					_ = os.Remove(filepath.Join(output, "extra"))
					_ = os.Remove(filepath.Join(output, "alias"))
				}
				defer restore()
				switch kind {
				case "extra":
					_ = os.WriteFile(filepath.Join(output, "extra"), nil, 0600)
				case "missing":
					_ = os.Remove(tarPath)
				case "changed":
					_ = os.WriteFile(tarPath, []byte("changed"), 0600)
				case "symlink":
					_ = os.Remove(tarPath)
					_ = os.Symlink(filepath.Join(output, "oracle.txt"), tarPath)
				case "hardlink":
					_ = os.Link(tarPath, filepath.Join(output, "alias"))
					_ = os.Remove(filepath.Join(output, "alias"))
					_ = os.Link(tarPath, filepath.Join(f.s.Root, "outside-output-alias"))
					defer os.Remove(filepath.Join(f.s.Root, "outside-output-alias"))
				case "size-audit":
					audit.FinalTAR.Size = 97 << 20
				case "digest-audit":
					audit.FinalTAR.Digest = "sha256:" + strings.Repeat("a", 64)
				case "duplicate-case":
					audit.Cases[1] = audit.Cases[0]
				}
				if _, e = ingestReferenceOutputs(context.Background(), f.s, base, audit); e == nil {
					t.Fatal("accepted mutated artifact", kind)
				}
			})
		}
		// Factual load checks content-addressed bytes, independent of scratch output.
		if _, e = LoadPreparedFinalReference(context.Background(), f.s, *a.Prepared()); e != nil {
			t.Fatal("factual load used scratch", e)
		}
		// A later malformed received unit blocks create-replay, not factual load.
		history := f.s.Path("state", "work", workFile(f.s, f.request.Label))
		_ = os.MkdirAll(filepath.Dir(history), 0700)
		_ = os.WriteFile(history, []byte("{}"), 0600)
		if _, e = PrepareFinalReference(context.Background(), f.s, req, f.now); e == nil {
			t.Fatal("history replay accepted")
		}
		if _, e = LoadPreparedFinalReference(context.Background(), f.s, *a.Prepared()); e != nil {
			t.Fatal("factual load recertified history", e)
		}

	})
	t.Run("failed-operation-retained", func(t *testing.T) {
		for _, mode := range []string{"exit", "remove_fail"} {
			t.Run(mode, func(t *testing.T) {
				if e := os.WriteFile(sel.ModeFile, []byte(mode), 0600); e != nil {
					t.Fatal(e)
				}
				defer os.WriteFile(sel.ModeFile, nil, 0600)
				f := definitionFixtureNew(t)
				ref := writeDefinition(t, f)
				req := PrepareReferenceRequest{ref, f.request.Label, "failed", sel.Policy}
				a, e := PrepareFinalReference(context.Background(), f.s, req, f.now)
				if e == nil || a == nil || a.Prepared() != nil || a.Outcome() == nil {
					t.Fatal("failure lost/returned Prepared", a, e)
				}
				if _, e = LoadPreparedFinalReference(context.Background(), f.s, *a.Outcome()); e == nil {
					t.Fatal("unknown became prepared")
				}
				b, e := PrepareFinalReference(context.Background(), f.s, req, f.now)
				if e == nil || b == nil || b.Prepared() != nil {
					t.Fatal("failure replay restarted", b, e)
				}
			})
		}
	})
	t.Run("conservative-policy-window-before-state", func(t *testing.T) {
		f := definitionFixtureNew(t)
		ref := writeDefinition(t, f)
		ctx, c := context.WithTimeout(context.Background(), 95*time.Second)
		defer c()
		a, e := PrepareFinalReference(ctx, f.s, PrepareReferenceRequest{ref, f.request.Label, "too-small", sel.Policy}, f.now)
		if e == nil || a != nil || !strings.Contains(e.Error(), "policy_window_refused") {
			t.Fatal(a, e)
		}
		if _, e = os.Lstat(f.s.Path("state", library.ReferencePreparationStateDir)); !os.IsNotExist(e) {
			t.Fatal("reserved before capacity", e)
		}
	})
	t.Run("orphan-reservation", func(t *testing.T) {
		f := definitionFixtureNew(t)
		ref := writeDefinition(t, f)
		u := hint.EvalRef(f.s.Key(), "work-unit", f.request.Label)
		op := hint.EvalRef(f.s.Key(), ReferencePreparationVersion, u+"\x00one")
		if e := os.MkdirAll(f.s.Path("state", library.ReferencePreparationStateDir, u, op), 0700); e != nil {
			t.Fatal(e)
		}
		if a, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy}, f.now); e == nil || a != nil {
			t.Fatal("resumed orphan", a, e)
		}
	})
	t.Run("pending-other-operation", func(t *testing.T) {
		f := definitionFixtureNew(t)
		ref := writeDefinition(t, f)
		u := hint.EvalRef(f.s.Key(), "work-unit", f.request.Label)
		op := hint.EvalRef(f.s.Key(), "other", "pending")
		_ = os.MkdirAll(f.s.Path("state", library.ReferencePreparationStateDir, u, op), 0700)
		if _, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy}, f.now); e == nil {
			t.Fatal("ignored pending")
		}
	})
}

// One bounded child build hosts all project positives/negatives. The production
// catalogue stays empty. Its replacement uses literal owned Unix endpoint/node pins.
func TestProjectReferencePublicPolicyUnixPersistence(t *testing.T) {
	if *referenceFixture != "" {
		t.Skip("parent only")
	}
	scratch, e := os.MkdirTemp("/tmp", "hint-project-reference-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(scratch)
	scratch, e = filepath.EvalSymlinks(scratch)
	if e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(scratch, "engine.sock")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(socket, 0600); e != nil {
		t.Fatal(e)
	}
	cli, fake := referenceengine.Start(t, "", l)
	defer cli.Close()
	var st unix.Stat_t
	if e = unix.Lstat(socket, &st); e != nil {
		t.Fatal(e)
	}
	pin := map[string]any{"Device": uint64(st.Dev), "Inode": st.Ino, "Mode": uint32(st.Mode), "UID": st.Uid, "GID": st.Gid, "Size": st.Size, "MTimeSec": st.Mtim.Sec, "MTimeNS": st.Mtim.Nsec, "CTimeSec": st.Ctim.Sec, "CTimeNS": st.Ctim.Nsec}
	pinRaw, _ := json.Marshal(pin)
	selector := filepath.Join(scratch, "selector.json")
	modeFile := filepath.Join(scratch, "mode")
	if e = os.WriteFile(modeFile, nil, 0600); e != nil {
		t.Fatal(e)
	}
	fake.BeforeCreate = func(bool) {
		b, e := os.ReadFile(modeFile)
		if e != nil {
			t.Error(e)
			return
		}
		fake.Mu.Lock()
		fake.Mode = string(b)
		fake.Mu.Unlock()
	}

	// The externally frozen source SHA is not embedded into its own source.
	catalogue := fmt.Sprintf(`package runner
import("context";"encoding/json";"os";"time")
var referencePreparationPolicies=[]referencePolicyEntry{}
func init(){
 var pin referenceSocketPin;if json.Unmarshal([]byte(%q),&pin)!=nil{panic("fixture pin")}
 actual,e:=referenceSocketPinAt(context.Background(),%q);if e!=nil||pin!=actual{panic("fixture socket")}
 bundle,_,_,e:=referenceBundleIdentity();if e!=nil{panic("fixture bundle")}
 entry:=referencePolicyEntry{ID:"synthetic.project-reference-v1",BundleDigest:bundle,Endpoint:%q,Socket:pin,EngineRef:referenceDigest([]byte("synthetic-engine")),EngineVersion:"synthetic-1",EngineOS:"linux",EngineArchitecture:"aarch64",APIVersion:"1.56",Procedure:referencePreparationPolicy{identity:"synthetic.project-reference-v1",operation:90*time.Second,resolve:time.Second,commit:time.Second,build:time.Second,apply:time.Second,overhead:time.Millisecond,cleanup:300*time.Millisecond,archiveCap:1<<20,totalCap:4<<20}}
 referencePreparationPolicies=[]referencePolicyEntry{entry}
 raw,_:=referencePolicySources.ReadFile("reference_policy_catalogue.go")
 result,_:=json.Marshal(struct{Policy ReferencePreparationPolicyRef;CatalogueSHA string
ModeFile string}{ReferencePreparationPolicyRef{entry.ID,referenceEntryDigest(entry)},referenceDigest(raw),%q})
 file,e:=os.OpenFile(%q,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600);if e!=nil{panic("fixture exclusive selector")};if _,e=file.Write(result);e!=nil{panic("fixture selector")};if e=file.Close();e!=nil{panic("fixture selector close")}
}
`, string(pinRaw), socket, socket, modeFile, selector)
	root, e := filepath.Abs(filepath.Join("..", ".."))
	if e != nil {
		t.Fatal(e)
	}
	replacement := filepath.Join(scratch, "catalogue.go")
	if e = os.WriteFile(replacement, []byte(catalogue), 0600); e != nil {
		t.Fatal(e)
	}
	overlayRaw, _ := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root, "internal/evals/runner/reference_policy_catalogue.go"): replacement}})
	overlay := filepath.Join(scratch, "overlay.json")
	if e = os.WriteFile(overlay, overlayRaw, 0600); e != nil {
		t.Fatal(e)
	}
	original, e := os.ReadFile(filepath.Join(root, "internal/evals/runner/reference_policy_catalogue.go"))
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-race", "-count=1", "-overlay", overlay, "./internal/project", "-run", "^TestProjectReferenceOverlayChild$", "-args", "-reference-fixture", selector)
	cmd.Dir = root
	cache, e := os.UserCacheDir()
	if e != nil {
		t.Fatal(e)
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + scratch, "TMPDIR=" + os.TempDir(), "GOCACHE=" + filepath.Join(cache, "go-build"), "GOMODCACHE=" + filepath.Join(os.Getenv("HOME"), "go", "pkg", "mod"), "GOENV=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off"}
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("synthetic child %v\n%s", e, out)
	}
	var sel referenceSelector
	raw, e := os.ReadFile(selector)
	if e != nil || json.Unmarshal(raw, &sel) != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(catalogue))
	expected := "sha256:" + hex.EncodeToString(sum[:])
	if sel.CatalogueSHA != expected {
		t.Fatal("embedded source mismatch", sel.CatalogueSHA, expected)
	}
	after, _ := os.ReadFile(filepath.Join(root, "internal/evals/runner/reference_policy_catalogue.go"))
	if !reflect.DeepEqual(original, after) {
		t.Fatal("production catalogue changed")
	}
	fake.Mu.Lock()
	defer fake.Mu.Unlock()
	if fake.Builds != 3 || fake.Creates != 5 {
		t.Fatal("replay/reservations dispatched", fake.Builds, fake.Creates)
	}
	t.Logf("frozen overlay catalogue=%s bytes=%d source=%q overlay=%q; child public API -> owned Unix fake -> real persistence/reload PASS", expected, len(catalogue), catalogue, string(overlayRaw))
}
