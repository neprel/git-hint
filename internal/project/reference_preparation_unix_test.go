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
	Policy                 runner.ReferencePreparationPolicyRef
	CatalogueSHA           string
	ModeFile               string
	FaultFile, FaultMarker string
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
		req := PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy, nil}
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
		if id.Eligibility != "requires_process_local_acceptance_witness" || id.Authority != "unknown" || id.DefinitionRef != ref || id.Audit.FinalTAR.Digest == id.Definition.QualifiedInputIdentity.ReferencePatchArchive {
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
		if a.Acceptance() == nil {
			t.Fatal("no checkpoint witness")
		}
		if _, e := json.Marshal(a.Acceptance()); e == nil {
			t.Fatal("serialized witness")
		}
		if bare, e := PrepareFinalReference(context.Background(), f.s, req, f.now); e == nil || bare.Prepared() != nil {
			t.Fatal("bare replay upgraded", bare, e)
		}
		zeroReq := req
		zeroReq.Replay = &ReferencePreparationAcceptance{}
		if rejected, e := PrepareFinalReference(context.Background(), f.s, zeroReq, f.now); e == nil || rejected.Prepared() != nil {
			t.Fatal("zero witness replay", e)
		}
		altered := a.Acceptance()
		altered.outcome.RecordDigest = "sha256:" + strings.Repeat("d", 64)
		zeroReq.Replay = altered
		if rejected, e := PrepareFinalReference(context.Background(), f.s, zeroReq, f.now); e == nil || rejected.Prepared() != nil {
			t.Fatal("wrong outcome witness", e)
		}
		if a.Acceptance().outcome.RecordDigest == altered.outcome.RecordDigest {
			t.Fatal("mutable witness accessor")
		}
		req.Replay = a.Acceptance()
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
		outcomePath := f.s.Path(append(base, "outcome.json")...)
		originalOutcome, e := os.ReadFile(outcomePath)
		if e != nil {
			t.Fatal(e)
		}
		for _, kind := range []string{"aggregate", "input-hash", "slot-order", "output-digest"} {
			t.Run("factual-transcript-"+kind, func(t *testing.T) {
				var terminal referenceTerminal
				if decodeReferenceRecord(originalOutcome, base, "outcome", &terminal) != nil {
					t.Fatal("terminal fixture")
				}
				switch kind {
				case "aggregate":
					terminal.Audit.OracleOutputsDigest = "sha256:" + strings.Repeat("a", 64)
				case "input-hash":
					terminal.Audit.Cases[0].InputDigest = "sha256:" + strings.Repeat("b", 64)
					terminal.Audit.Cases[1].InputDigest = terminal.Audit.Cases[0].InputDigest
				case "slot-order":
					terminal.Audit.Cases[0], terminal.Audit.Cases[1] = terminal.Audit.Cases[1], terminal.Audit.Cases[0]
				case "output-digest":
					terminal.Audit.Cases[0].OutputDigest = "sha256:" + strings.Repeat("c", 64)
				}
				forged := referenceEncoded(base, "outcome", terminal)
				if e = os.WriteFile(outcomePath, forged, 0600); e != nil {
					t.Fatal(e)
				}
				defer os.WriteFile(outcomePath, originalOutcome, 0600)
				if _, e = LoadPreparedFinalReference(context.Background(), f.s, referenceRecordRef(base, "outcome", forged)); e == nil {
					t.Fatal("contradictory retained transcript loaded", kind)
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
	t.Run("actual-coordinator-late-link-never-upgrades", func(t *testing.T) {
		for _, mode := range []string{"late-link", "late-poison", "final-window"} {
			t.Run(mode, func(t *testing.T) {
				f := definitionFixtureNew(t)
				ref := writeDefinition(t, f)
				req := PrepareReferenceRequest{Definition: ref, Label: f.request.Label, OperationID: mode, Policy: sel.Policy}
				if e := os.WriteFile(sel.FaultFile, []byte(mode), 0600); e != nil {
					t.Fatal(e)
				}
				defer func() { _ = os.WriteFile(sel.FaultFile, nil, 0600); _ = os.Remove(sel.FaultMarker) }()
				a, e := PrepareFinalReference(context.Background(), f.s, req, f.now)
				if e == nil || a == nil || a.Prepared() != nil || a.Acceptance() != nil {
					t.Fatal("late write returned eligibility", a, e)
				}
				parts := strings.Split(a.Reservation().RecordRef, ".")
				base := []string{"state", library.ReferencePreparationStateDir, parts[0], parts[1]}
				raw, e := f.s.ReadStateBounded(context.Background(), referenceMetadataMax, append(base, "outcome.json")...)
				if e != nil {
					t.Fatal("test never reached durable coordinator outcome", e)
				}
				var terminal referenceTerminal
				if decodeReferenceRecord(raw, base, "outcome", &terminal) != nil || terminal.State != "prepared_reference_observed" {
					t.Fatal("not a durable ambiguous positive")
				}
				if mode == "late-poison" {
					if e = f.s.WriteStateImmutableContext(context.Background(), []byte("poison"), 64, append(base, "failure-marker.json")...); e == nil {
						t.Fatal("poison unavailable control did not activate")
					}
				}
				if mode == "final-window" {
					marker, e := os.ReadFile(sel.FaultMarker)
					if e != nil || string(marker) != "final-end-observed" {
						t.Fatal("success writer bypassed short final endpoint", string(marker), e)
					}
				}
				loaded, e := LoadPreparedFinalReference(context.Background(), f.s, referenceRecordRef(base, "outcome", raw))
				if e != nil || loaded.Identity().Eligibility != "requires_process_local_acceptance_witness" {
					t.Fatal("factual evidence shape", e)
				}
				b, e := PrepareFinalReference(context.Background(), f.s, req, f.now)
				if e == nil || b == nil || b.Prepared() != nil || b.Acceptance() != nil {
					t.Fatal("durable late positive upgraded on real replay", b, e)
				}
				t.Log("actual coordinator wrote positive bytes, returned error, and refused public replay without a witness", mode)
			})
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
				req := PrepareReferenceRequest{ref, f.request.Label, "failed", sel.Policy, nil}
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
		a, e := PrepareFinalReference(ctx, f.s, PrepareReferenceRequest{ref, f.request.Label, "too-small", sel.Policy, nil}, f.now)
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
		if a, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy, nil}, f.now); e == nil || a != nil {
			t.Fatal("resumed orphan", a, e)
		}
	})
	t.Run("pending-other-operation", func(t *testing.T) {
		f := definitionFixtureNew(t)
		ref := writeDefinition(t, f)
		u := hint.EvalRef(f.s.Key(), "work-unit", f.request.Label)
		op := hint.EvalRef(f.s.Key(), "other", "pending")
		_ = os.MkdirAll(f.s.Path("state", library.ReferencePreparationStateDir, u, op), 0700)
		if _, e := PrepareFinalReference(context.Background(), f.s, PrepareReferenceRequest{ref, f.request.Label, "one", sel.Policy, nil}, f.now); e == nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GOENV=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off"}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	query := exec.CommandContext(ctx, "go", "env", "-json", "GOCACHE", "GOMODCACHE")
	query.Env = env
	cacheRaw, e := query.Output()
	if e != nil {
		t.Fatal("offline cache query", e)
	}
	var caches map[string]string
	if json.Unmarshal(cacheRaw, &caches) != nil || !filepath.IsAbs(caches["GOCACHE"]) || !filepath.IsAbs(caches["GOMODCACHE"]) {
		t.Fatal("invalid configured cache paths")
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
	faultFile := filepath.Join(scratch, "fault-mode")
	faultMarker := filepath.Join(scratch, "fault-marker")
	if e = os.WriteFile(faultFile, nil, 0600); e != nil {
		t.Fatal(e)
	}
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
ModeFile,FaultFile,FaultMarker string}{ReferencePreparationPolicyRef{entry.ID,referenceEntryDigest(entry)},referenceDigest(raw),%q,%q,%q})
 file,e:=os.OpenFile(%q,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600);if e!=nil{panic("fixture exclusive selector")};if _,e=file.Write(result);e!=nil{panic("fixture selector")};if e=file.Close();e!=nil{panic("fixture selector close")}
}
`, string(pinRaw), socket, socket, modeFile, faultFile, faultMarker, selector)
	root, e := filepath.Abs(filepath.Join("..", ".."))
	if e != nil {
		t.Fatal(e)
	}
	replacement := filepath.Join(scratch, "catalogue.go")
	if e = os.WriteFile(replacement, []byte(catalogue), 0600); e != nil {
		t.Fatal(e)
	}
	// Test-build faults run inside the ACTUAL shared writer after its durable
	// link/fsync/identity checks. No production callback or store source edit exists.
	storePath := filepath.Join(root, "internal/store/bounded_unix.go")
	storeRaw, e := os.ReadFile(storePath)
	if e != nil {
		t.Fatal(e)
	}
	writer := string(storeRaw)
	head := fmt.Sprintf(`
 modeRaw,_:=os.ReadFile(%q)
 if string(modeRaw)=="late-poison"{if _,e:=os.Stat(%q);e==nil{return ErrBoundedState}}
 `, faultFile, faultMarker)
	signature := "func writeBoundedState(ctx context.Context, root string, parts []string, data []byte) error {"
	if strings.Count(writer, signature) != 1 {
		t.Fatal("writer fixture anchor changed")
	}
	writer = strings.Replace(writer, signature, signature+head, 1)
	fault := fmt.Sprintf(`
 if parts[len(parts)-1]=="outcome.json" && strings.Contains(strings.Join(parts,"/"),"reference-preparations/") {
 modeRaw,_:=os.ReadFile(%q);mode:=string(modeRaw)
 if mode=="late-poison"{if e:=os.WriteFile(%q,[]byte("poison-unavailable"),0600);e!=nil{return e}}
 if mode=="final-window"{deadline,ok:=ctx.Deadline();if !ok||time.Until(deadline)>5*time.Second{_ = os.WriteFile(%q,[]byte("wrong-long-window"),0600);return context.DeadlineExceeded};_ = os.WriteFile(%q,[]byte("final-end-observed"),0600);<-ctx.Done();return ctx.Err()}
 if mode=="late-link"||mode=="late-poison"{return context.DeadlineExceeded}
 }
 `, faultFile, faultMarker, faultMarker, faultMarker)
	if strings.Count(writer, "return ctx.Err()\n}") != 1 {
		t.Fatal("post-link fixture anchor changed")
	}
	writer = strings.Replace(writer, "return ctx.Err()\n}", fault+"return ctx.Err()\n}", 1)
	writer = strings.Replace(writer, "\"strings\"", "\"strings\"\n\"time\"", 1)
	storeReplacement := filepath.Join(scratch, "bounded_unix.go")
	if e = os.WriteFile(storeReplacement, []byte(writer), 0600); e != nil {
		t.Fatal(e)
	}
	projectPath := filepath.Join(root, "internal/project/reference_preparation.go")
	projectRaw, e := os.ReadFile(projectPath)
	if e != nil {
		t.Fatal(e)
	}
	timing := strings.Replace(string(projectRaw), "const referenceFinalCommitMax = 60 * time.Second", "const referenceFinalCommitMax = 3 * time.Second", 1)
	if timing == string(projectRaw) {
		t.Fatal("private final cap fixture anchor changed")
	}
	projectReplacement := filepath.Join(scratch, "reference_preparation.go")
	if e = os.WriteFile(projectReplacement, []byte(timing), 0600); e != nil {
		t.Fatal(e)
	}
	overlayRaw, _ := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root, "internal/evals/runner/reference_policy_catalogue.go"): replacement, storePath: storeReplacement, projectPath: projectReplacement}})
	overlay := filepath.Join(scratch, "overlay.json")
	if e = os.WriteFile(overlay, overlayRaw, 0600); e != nil {
		t.Fatal(e)
	}
	original, e := os.ReadFile(filepath.Join(root, "internal/evals/runner/reference_policy_catalogue.go"))
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-race", "-count=1", "-overlay", overlay, "./internal/project", "-run", "^TestProjectReferenceOverlayChild$", "-v", "-args", "-reference-fixture", selector)
	cmd.Dir = root

	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + scratch, "TMPDIR=" + os.TempDir(), "GOCACHE=" + caches["GOCACHE"], "GOMODCACHE=" + caches["GOMODCACHE"], "GOENV=off", "GOFLAGS=", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOWORK=off"}
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
	if fake.Builds != 6 || fake.Creates != 14 {
		t.Fatal("replay/reservations dispatched", fake.Builds, fake.Creates)
	}
	t.Logf("child_output=%q store_overlay=%q project_overlay=%q", string(out), writer, timing)
	t.Logf("frozen overlay catalogue=%s bytes=%d source=%q overlay=%q; child public API -> owned Unix fake -> real persistence/reload PASS", expected, len(catalogue), catalogue, string(overlayRaw))
}
