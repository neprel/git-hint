package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

func project(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	testkit.WriteFile(t, dir, "a.go", "package a\n")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func evalRecord(id string, version int, created string, refs ...string) map[string]any {
	d := func(i int) string {
		if i < len(refs) {
			return refs[i]
		}
		return "sha256:" + strings.Repeat(fmt.Sprint(i%10), 64)
	}
	prev := any(nil)
	if version > 1 {
		prev = fmt.Sprintf("%s@%d", id, version-1)
	}
	return map[string]any{
		"record": "eval", "manifest_version": 1, "eval_id": id, "version": version, "previous": prev, "created_at": created,
		"episode_id": "ep-0123456789abcdef", "family": "bug_fix",
		"repositories": []any{map[string]any{"repo": "app", "base_tree": d(0), "base_archive": d(1)}},
		"instruction":  d(2),
		"requirements": []any{map[string]any{"req_id": "R1", "kind": "behavior", "origin": "request", "text": d(3), "evidence": []any{}, "checks": []any{"C1"}}},
		"verifier":     map[string]any{"archive": d(4), "checks": []any{map[string]any{"check_id": "C1", "origin": "result_test"}}},
		"environment":  map[string]any{"image": "python:3.12-slim@sha256:" + strings.Repeat("0", 64), "workdir": "/app", "test_command": "tests/test.sh"},
		"dependencies": []any{}, "state": "packaged", "reasons": []any{},
		"qualification": map[string]any{"policy": "q1", "checks": []any{}}, "override": nil,
	}
}

func TestObjectsAreContentAddressedAndChecked(t *testing.T) {
	l := Open(project(t), 0)
	d, err := l.PutObject([]byte("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := l.PutObject([]byte("evidence"))
	if err != nil || d2 != d {
		t.Fatalf("repeat import: %v %s", err, d2)
	}
	p, _ := l.objectPath(d)
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("object permissions: %v %v", err, fi.Mode())
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetObject(d); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupted object: %v", err)
	}
	if _, err := l.PutObject([]byte("evidence")); err != nil { // a new import repairs the copy
		t.Fatal(err)
	}
	if b, err := l.GetObject(d); err != nil || string(b) != "evidence" {
		t.Fatalf("repaired object: %v", err)
	}
	if _, err := l.GetObject("sha256:" + strings.Repeat("a", 64)); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing object: %v", err)
	}
}

func TestManifestsAreImmutable(t *testing.T) {
	l := Open(project(t), 0)
	rec := evalRecord("ev-0123456789abcdef", 1, "2026-10-01T00:00:00Z")
	name, err := l.PutManifest("eval", rec)
	if err != nil || name != "ev-0123456789abcdef@1" {
		t.Fatalf("%v %s", err, name)
	}
	if _, err := l.PutManifest("eval", rec); err != nil {
		t.Fatalf("identical record again: %v", err)
	}
	rec["state"] = "validated"
	if _, err := l.PutManifest("eval", rec); !errors.Is(err, ErrImmutable) {
		t.Fatalf("changed record under the same version: %v", err)
	}
	if _, err := l.PutManifest("eval", evalRecord("ev-0123456789abcdef", 2, "2026-10-02T00:00:00Z")); err != nil {
		t.Fatalf("new version: %v", err)
	}
	bad := evalRecord("ev-0123456789abcdef", 3, "2026-10-02T00:00:00Z")
	bad["instruction"] = "Fix the bug"
	if _, err := l.PutManifest("eval", bad); err == nil {
		t.Fatal("an invalid record was stored")
	}
}

func TestConcurrentWriters(t *testing.T) {
	s := project(t)
	a, b := Open(s, 0), Open(s, 0) // two handles, as two callers
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 50; i++ {
		for _, l := range []*Library{a, b} {
			wg.Add(1)
			go func(l *Library, i int) {
				defer wg.Done()
				if _, err := l.PutObject([]byte(fmt.Sprint("obj", i%10))); err != nil {
					errs <- err
				}
				if _, err := l.PutManifest("eval", evalRecord("ev-0123456789abcdef", 1, "2026-10-01T00:00:00Z")); err != nil {
					errs <- err
				}
			}(l, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	names, _ := a.Manifests("eval")
	if len(names) != 1 {
		t.Fatalf("manifests: %v", names)
	}
}

func TestExclusionRemovalStopsWrites(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	if _, err := l.PutObject([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, ".git", "info", "exclude"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutObject([]byte("y")); !errors.Is(err, store.ErrNotExcluded) {
		t.Fatalf("write after exclusion removal: %v", err)
	}
}

func TestSymlinkEscapeIsRefused(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	outside := t.TempDir()
	if err := os.MkdirAll(s.Path("state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, s.Path("state", "evals")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutObject([]byte("x")); !errors.Is(err, ErrEscape) {
		t.Fatalf("write through a link out of the store: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatal("data reached the link target")
	}
}

func TestQuotaStopsWrites(t *testing.T) {
	l := Open(project(t), 64) // disk-full stand-in: a tiny quota
	if _, err := l.PutObject([]byte(strings.Repeat("a", 40))); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PutObject([]byte(strings.Repeat("b", 40))); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
}

func TestInterruptedWriteAndRestart(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	d, _ := l.PutObject([]byte("kept"))
	p, _ := l.objectPath(d)
	tmp := filepath.Join(filepath.Dir(p), ".tmp-interrupted")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := Open(s, 0) // a new process
	n, err := l2.Recover()
	if err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	if b, err := l2.GetObject(d); err != nil || string(b) != "kept" {
		t.Fatalf("complete object lost: %v", err)
	}
}

func TestRestartCannotResetBudget(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	job := "jb-0123456789abcdef"
	if err := l.StartJob(job, "model", Budget{Requests: 3, Seconds: 60}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Consume(job, 1, 0, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	l2 := Open(s, 0) // restart
	if err := l2.StartJob(job, "model", Budget{Requests: 100, Seconds: 600}); !errors.Is(err, ErrJobExists) {
		t.Fatalf("restart set a new budget: %v", err)
	}
	if err := l2.Consume(job, 1, 0, 0, 1); err == nil {
		t.Fatal("consumption beyond the recorded budget")
	}
	st, _ := l2.Job(job)
	if ok, reason := st.Remaining(); ok || reason != "request_limit" {
		t.Fatalf("remaining: %v %s", ok, reason)
	}
	if err := l.StartJob("jb-1111111111111111", "model", Budget{Requests: 3}); err == nil {
		t.Fatal("a job without a time limit was started")
	}
}

func TestTornJournalStopsOpenJobs(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	job := "jb-0123456789abcdef"
	if err := l.StartJob(job, "trial", Budget{Trials: 5, Seconds: 600}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(l.path("journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"at":"2026-10-17T00:00:00Z","job":"jb-0123456789abcdef","type":"cons`) // crash mid-line
	f.Close()
	st, err := l.Job(job)
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := st.Remaining(); ok || reason != "journal_damaged" {
		t.Fatalf("torn journal: %v %s", ok, reason)
	}
}

func TestCacheCleanupKeepsEvidence(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	d, _ := l.PutObject([]byte("evidence"))
	if err := os.MkdirAll(s.Path("cache", "evals"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(s, 0).GetObject(d); err != nil {
		t.Fatalf("evidence lost after cache cleanup: %v", err)
	}
}

func TestRetentionForgetAndPurge(t *testing.T) {
	s := project(t)
	l := Open(s, 0)
	old, _ := l.PutObject([]byte("old instruction"))
	pinned, _ := l.PutObject([]byte("pinned instruction"))
	fresh, _ := l.PutObject([]byte("fresh instruction"))
	orphan, _ := l.PutObject([]byte("orphan"))
	mk := func(id, created, ref string) {
		r := evalRecord(id, 1, created)
		r["instruction"] = ref
		if _, err := l.PutManifest("eval", r); err != nil {
			t.Fatal(err)
		}
	}
	mk("ev-000000000000000a", "2026-01-01T00:00:00Z", old)
	mk("ev-000000000000000b", "2026-01-01T00:00:00Z", pinned)
	mk("ev-000000000000000c", "2026-10-16T00:00:00Z", fresh)
	if err := l.Pin("ev-000000000000000b@1", true); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordExternal(External{Kind: "image", Name: "hint-eval-a"}); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Prune(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), 90)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Manifests != 1 || rep.Objects < 2 || len(rep.Kept) != 1 || len(rep.External) != 1 {
		t.Fatalf("prune: %+v", rep)
	}
	for d, want := range map[string]bool{old: false, orphan: false, pinned: true, fresh: true} {
		_, err := l.GetObject(d)
		if (err == nil) != want {
			t.Errorf("object %s kept=%v", d[:16], err == nil)
		}
	}
	rep, err = l.Forget([]string{"ev-000000000000000c"})
	if err != nil || rep.Manifests != 1 {
		t.Fatalf("forget: %+v %v", rep, err)
	}
	if _, err := l.GetObject(fresh); !errors.Is(err, ErrMissing) {
		t.Fatal("forgotten eval keeps its objects")
	}
	rep, err = l.Purge()
	if err != nil || len(rep.External) != 1 {
		t.Fatalf("purge: %+v %v", rep, err)
	}
	if _, err := os.Stat(l.path()); !os.IsNotExist(err) {
		t.Fatal("library still exists after purge")
	}
	if _, err := os.Stat(s.Path("state", "project.json")); err != nil {
		t.Fatal("purge removed project state outside the library")
	}
}
