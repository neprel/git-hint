package runner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrelaunchCapturePrecedesStartAndSharesEnclosureClock(t *testing.T) {
	var mu sync.Mutex
	var events []string
	var artifactDigest string
	r, _ := liveFake(t, liveFakeOpts{prelaunch: true, digest: true, requestInWait: true,
		engineEvent: func(e string) { mu.Lock(); defer mu.Unlock(); events = append(events, e) },
		artifactCheck: func(p string) {
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			h := sha256.Sum256(b)
			artifactDigest = "sha256:" + hex.EncodeToString(h[:])
		},
	})
	var c PrelaunchTreeCapture
	b, _ := json.Marshal(r.Container["prelaunch_tree"])
	if json.Unmarshal(b, &c) != nil {
		t.Fatal("missing capture")
	}
	a := persisted(t, r).AgentRun
	if c.State != "complete" || c.Clock != "ledger_anchor_monotonic_ms" || c.Digest != artifactDigest || a == nil || a.EnclosureStartMs == nil || c.StartedMs < 0 || c.FinishedMs < c.StartedMs || c.FinishedMs > *a.EnclosureStartMs || c.ExecutionDigest != a.ExecutionDigest {
		t.Fatalf("capture %+v run %+v", c, a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) < 4 || !reflect.DeepEqual(events[:4], []string{"PUT archive", "GET archive", "agent start", "GET archive"}) {
		t.Fatalf("ordering %v", events)
	}
}

func TestPrelaunchFailuresPreventAgentStartAndModelRequests(t *testing.T) {
	for name, opts := range map[string]liveFakeOpts{
		"read": {archiveFailure: true}, "tree": {evilTree: true}, "unknown execution": {}, "existing artifact": {preexistingArtifact: true},
	} {
		t.Run(name, func(t *testing.T) {
			opts.prelaunch, opts.requestInWait = true, true
			opts.digest = name != "unknown execution"
			var mu sync.Mutex
			started := false
			opts.engineEvent = func(e string) {
				if e == "agent start" {
					mu.Lock()
					started = true
					mu.Unlock()
				}
			}
			opts.artifactCheck = func(p string) {
				if opts.preexistingArtifact {
					b, e := os.ReadFile(p)
					if e != nil || string(b) != "foreign artifact" {
						t.Fatal("capture removed foreign artifact")
					}
				} else if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("refused capture left artifact: %v", err)
				}
			}
			r, c := liveFake(t, opts)
			mu.Lock()
			defer mu.Unlock()
			if started || c.Requests() != 0 || r.ExceptionInfo == nil {
				t.Fatalf("started %v requests %d result %+v", started, c.Requests(), r)
			}
			if a := persisted(t, r).AgentRun; opts.digest && (a == nil || a.Outcome != AgentRefusedBeforeLaunch || a.EnclosureStartMs != nil) {
				t.Fatalf("enclosure %+v", a)
			}
		})
	}
}

func TestPrelaunchDefaultDoesNotCapture(t *testing.T) {
	r, _ := liveFake(t, liveFakeOpts{digest: true, requestInWait: true, artifactCheck: func(p string) {
		if _, e := os.Stat(p); !os.IsNotExist(e) {
			t.Fatal("default capture")
		}
	}})
	if r.Container["prelaunch_tree"] != nil {
		t.Fatal("default metadata changed")
	}
}

func TestMaterializePrelaunchValidatesBeforeWritesAndSkipsAliases(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "capture.tar")
	raw := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755},
		&tar.Header{Name: "app/a.go", Typeflag: tar.TypeReg, Mode: 0644},
		&tar.Header{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "a.go"},
		&tar.Header{Name: "app/hard", Typeflag: tar.TypeLink, Linkname: "app/a.go"},
		&tar.Header{Name: "app/long-" + strings.Repeat("a", 150), Typeflag: tar.TypeReg, Mode: 0644})
	digest, err := freezeTree(bytes.NewReader(raw.Bytes()), artifact)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	files, err := MaterializePrelaunchTree(artifact, digest, dest)
	if err != nil || len(files) != 2 || files[0] != "a.go" {
		t.Fatalf("files %v %v", files, err)
	}
	for _, alias := range []string{"link", "hard"} {
		if _, err := os.Lstat(filepath.Join(dest, alias)); !os.IsNotExist(err) {
			t.Fatal("materialized alias")
		}
	}
	// An occupied destination remains untouched, even when the artifact is valid.
	if _, err := MaterializePrelaunchTree(artifact, digest, dest); err == nil {
		t.Fatal("occupied destination accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "a.go")); string(b) != "x" {
		t.Fatal("foreign file changed")
	}
}

func TestMaterializePrelaunchRejectsMalformedOrChangedArchive(t *testing.T) {
	good := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755}, &tar.Header{Name: "app/a", Typeflag: tar.TypeReg, Mode: 0644}).Bytes()
	evil := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir}, &tar.Header{Name: "app/../outside", Typeflag: tar.TypeReg}).Bytes()
	links := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir}, &tar.Header{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "/outside"}).Bytes()
	duplicate := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir}, &tar.Header{Name: "app/a", Typeflag: tar.TypeReg}, &tar.Header{Name: "app/a", Typeflag: tar.TypeReg}).Bytes()
	var oversized bytes.Buffer
	headerWriter := tar.NewWriter(&oversized)
	if err := headerWriter.WriteHeader(&tar.Header{Name: "app/huge", Typeflag: tar.TypeReg, Mode: 0644, Size: maxTreeBytes + 1}); err != nil {
		t.Fatal(err)
	}
	fifo := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir}, &tar.Header{Name: "app/fifo", Typeflag: tar.TypeFifo}).Bytes()
	belowLink := tarOf(t, &tar.Header{Name: "app/", Typeflag: tar.TypeDir}, &tar.Header{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "inside"}, &tar.Header{Name: "app/link/file", Typeflag: tar.TypeReg}).Bytes()
	for name, body := range map[string][]byte{"path": evil, "link": links, "duplicate": duplicate, "fifo": fifo, "below link": belowLink, "declared size": oversized.Bytes(), "truncated": good[:550], "trailing": append(bytes.Clone(good), 'x'), "digest": good} {
		t.Run(name, func(t *testing.T) {
			artifact := filepath.Join(t.TempDir(), "capture.tar")
			os.WriteFile(artifact, body, 0600)
			h := sha256.Sum256(body)
			digest := "sha256:" + hex.EncodeToString(h[:])
			if name == "digest" {
				digest = "sha256:" + strings.Repeat("0", 64)
			}
			dest := t.TempDir()
			if _, err := MaterializePrelaunchTree(artifact, digest, dest); err == nil {
				t.Fatal("accepted invalid artifact")
			}
			entries, _ := os.ReadDir(dest)
			if len(entries) != 0 {
				t.Fatal("wrote before validation")
			}
		})
	}
	artifact := filepath.Join(t.TempDir(), "link.tar")
	os.Symlink("missing", artifact)
	if _, err := MaterializePrelaunchTree(artifact, "sha256:"+strings.Repeat("0", 64), t.TempDir()); err == nil {
		t.Fatal("symlink artifact accepted")
	}
}

func TestMaterializePrelaunchRejectsOversizeAndSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "large.tar")
	f, err := os.Create(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxPrelaunchArchiveBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := MaterializePrelaunchTree(artifact, "sha256:"+strings.Repeat("0", 64), t.TempDir()); err == nil {
		t.Fatal("oversize artifact accepted")
	}
	foreign := t.TempDir()
	alias := filepath.Join(dir, "destination")
	os.Symlink(foreign, alias)
	if _, err := MaterializePrelaunchTree(artifact, "sha256:"+strings.Repeat("0", 64), alias); err == nil {
		t.Fatal("symlink destination accepted")
	}
	entries, _ := os.ReadDir(foreign)
	if len(entries) != 0 {
		t.Fatal("foreign destination changed")
	}
}

func TestPrelaunchUsesCounterMonotonicAnchorDespiteWallClockStep(t *testing.T) {
	var mu sync.Mutex
	mono := time.Duration(-100) * time.Millisecond
	wallCalls := 0
	clock := &ledgerClock{
		mono: func() time.Duration { mu.Lock(); defer mu.Unlock(); mono += 100 * time.Millisecond; return mono },
		wall: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			wallCalls++
			return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(wallCalls) * time.Hour)
		},
	}
	r, _ := liveFake(t, liveFakeOpts{prelaunch: true, digest: true, requestInWait: true, clock: clock})
	var c PrelaunchTreeCapture
	raw, _ := json.Marshal(r.Container["prelaunch_tree"])
	json.Unmarshal(raw, &c)
	a := persisted(t, r).AgentRun
	if c.StartedMs != 100 || c.FinishedMs != 200 || a == nil || a.EnclosureStartMs == nil || *a.EnclosureStartMs != 300 {
		t.Fatalf("capture %+v enclosure %+v", c, a)
	}
}
