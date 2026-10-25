//go:build unix

package trees

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type receiptPairFixture struct {
	limits Limits
	a, b   SelectedReceipt
	x, y   SourceViewReceipt
}

func (p receiptPairFixture) copy() receiptPairFixture {
	p.a.Selected = append([]SelectedPathFact{}, p.a.Selected...)
	p.b.Selected = append([]SelectedPathFact{}, p.b.Selected...)
	p.a.Stages = append([]IndexStageFact{}, p.a.Stages...)
	p.b.Stages = append([]IndexStageFact{}, p.b.Stages...)
	p.x.CandidatePaths = append([]string{}, p.x.CandidatePaths...)
	p.y.CandidatePaths = append([]string{}, p.y.CandidatePaths...)
	p.x.Facts = append([]SourceFact{}, p.x.Facts...)
	p.y.Facts = append([]SourceFact{}, p.y.Facts...)
	return p
}
func (p receiptPairFixture) validate() error {
	return ValidateSelectedSourcePair(p.limits, p.a, p.b, p.x, p.y)
}
func openValidationView(t *testing.T, shape string) (*SelectedSourceView, Limits) {
	t.Helper()
	v, root := sourceViewFixture(t)
	if e := v.Close(); e != nil {
		t.Fatal(e)
	}
	l := v.session.library
	lim := Limits{MaxFileBytes: 64, MaxTotalBytes: 128, MaxFiles: 4}
	switch shape {
	case "mode_untracked":
		if e := os.Chmod(filepath.Join(root, "nested/input"), 0755); e != nil {
			t.Fatal(e)
		}
		testkit.WriteFile(t, root, "untracked", "synthetic untracked")
	case "absent":
		testkit.WriteFile(t, root, ".git/info/exclude", ".hint/\n")
		for _, name := range []string{".gitignore", "nested/input"} {
			if e := os.Remove(filepath.Join(root, name)); e != nil {
				t.Fatal(e)
			}
		}
	case "empty":
		root = filepath.Join(filepath.Dir(root), "empty-root")
		if e := os.Mkdir(root, 0700); e != nil {
			t.Fatal(e)
		}
		testkit.Git(t, root, "init", "-q")
		testkit.Git(t, root, "-c", "user.name=Synthetic", "-c", "user.email=synthetic@example.invalid", "commit", "--allow-empty", "-qm", "synthetic empty")
		s, _, e := store.Init(root, store.InitOptions{})
		if e != nil {
			t.Fatal(e)
		}
		l = library.Open(s, 0)
	}
	actual, e := OpenSelectedSourceView(context.Background(), l, root, SourceViewOptions{Limits: lim})
	if e != nil {
		t.Fatal(e)
	}
	return actual, lim
}
func capturedValidationPair(t *testing.T, shape string) receiptPairFixture {
	t.Helper()
	actual, lim := openValidationView(t, shape)
	defer func() {
		if e := actual.Close(); e != nil {
			t.Error(e)
		}
	}()
	a, e := actual.Capture(context.Background())
	if e != nil {
		t.Fatalf("actual shape=%s pass=1 phase=%s chain_nil=%t frozen_nil=%t lengths=%d/%d: %v", shape, actual.checkPhase, actual.chain == nil, actual.firstChain == nil, len(actual.chain), len(actual.firstChain), e)
	}
	b, e := actual.Capture(context.Background())
	if e != nil {
		t.Fatalf("actual shape=%s pass=2 phase=%s chain_nil=%t frozen_nil=%t lengths=%d/%d: %v", shape, actual.checkPhase, actual.chain == nil, actual.firstChain == nil, len(actual.chain), len(actual.firstChain), e)
	}
	p := receiptPairFixture{lim, a.Selected().Receipt(), b.Selected().Receipt(), a.Sources(), b.Sources()}
	if e := p.validate(); e != nil {
		t.Fatalf("actual %s pair refused: %v", shape, e)
	}
	return p
}
func TestResolveStrictCaptureLimitsSharedDefaults(t *testing.T) {
	for _, lim := range []Limits{{}, {MaxFiles: 2, MaxFileBytes: 20, MaxTotalBytes: 40}, {MaxFiles: 20000, MaxFileBytes: 20 << 20, MaxTotalBytes: 200 << 20}, {MaxFiles: -1}, {MaxFileBytes: -1}, {MaxTotalBytes: -1}} {
		got, e := ResolveStrictCaptureLimits(lim)
		want, we := strictEffectiveLimits(lim)
		if got != want || (e == nil) != (we == nil) {
			t.Fatal("strict normalization fork")
		}
	}
	got, e := ResolveStrictCaptureLimits(Limits{})
	if e != nil || got != (Limits{8 << 20, 64 << 20, 10000}) {
		t.Fatal("literal strict defaults changed")
	}
}
func TestValidateSelectedSourcePairActualShapes(t *testing.T) {
	for _, shape := range []string{"mode_untracked", "absent", "empty"} {
		t.Run(shape, func(t *testing.T) {
			p := capturedValidationPair(t, shape)
			saved := p.copy()
			if e := p.validate(); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(p, saved) {
				t.Fatal("pure validation mutated receipts")
			}
			if shape == "mode_untracked" {
				seenMode, seenUntracked := false, false
				for _, f := range p.a.Selected {
					if f.Path == "nested/input" {
						seenMode = f.Mode == "100755"
					}
					if f.Path == "untracked" {
						seenUntracked = !f.Indexed
					}
				}
				if !seenMode || !seenUntracked {
					t.Fatal("actual unstaged/untracked controls missing")
				}
				for _, s := range p.a.Stages {
					if s.Path == "nested/input" && s.Mode != "100644" {
						t.Fatal("index changed with worktree")
					}
				}
			}
			if shape == "absent" && (p.a.PresentCount != 0 || p.a.IndexedAbsentCount != 3) {
				t.Fatal("genuine indexed-absent form missing")
			}
			if shape == "empty" && (len(p.a.Selected) != 0 || len(p.a.Stages) != 0 || p.a.PresentCount != 0 || p.a.IndexedAbsentCount != 0) {
				t.Fatal("genuine empty form missing")
			}
		})
	}
}

// Refresh only declared digests for malformed synthetic mutations. Semantic
// assertions below stay independent of the predicate under test.
func rebindPairDigests(p *receiptPairFixture) {
	for _, pair := range []struct {
		s *SelectedReceipt
		r *SourceViewReceipt
	}{{&p.a, &p.x}, {&p.b, &p.y}} {
		s, r := pair.s, pair.r
		s.SelectedInventoryDigest, s.StageInventoryDigest = selectedReceiptDigests(*s)
		var files []File
		for _, f := range s.Selected {
			if f.State == SelectedRegularPresent {
				files = append(files, File{f.Path, f.Mode, f.Size, f.ObjectDigest})
			}
		}
		raw, _ := treeManifestBytes(files)
		s.TreeDigest = library.Digest(raw)
		r.SelectedInventoryDigest = s.SelectedInventoryDigest
		r.StageInventoryDigest = s.StageInventoryDigest
		r.TreeDigest = s.TreeDigest
		r.SourceDigest = sourceFactsDigest(r.Facts)
	}
}
func bothSelected(p *receiptPairFixture, fn func(*SelectedReceipt)) { fn(&p.a); fn(&p.b) }
func bothSource(p *receiptPairFixture, fn func(*SourceViewReceipt)) { fn(&p.x); fn(&p.y) }
func TestValidateSelectedSourcePairMalformedMutations(t *testing.T) {
	base := capturedValidationPair(t, "mode_untracked")
	tests := []struct {
		name   string
		mutate func(*receiptPairFixture)
		rebind bool
	}{
		{"negative_limits", func(p *receiptPairFixture) { p.limits.MaxFiles = -1 }, false},
		{"tighter_count", func(p *receiptPairFixture) { p.limits.MaxFiles = 2 }, false},
		{"tighter_file", func(p *receiptPairFixture) { p.limits.MaxFileBytes = 1 }, false},
		{"tighter_total", func(p *receiptPairFixture) { p.limits.MaxTotalBytes = 1 }, false},
		{"selected_version", func(p *receiptPairFixture) { p.a.Version = "unknown" }, false},
		{"selected_pass", func(p *receiptPairFixture) { p.a.Pass = 2 }, false},
		{"selected_nil", func(p *receiptPairFixture) { p.a.Selected = nil }, false},
		{"stage_nil", func(p *receiptPairFixture) { p.a.Stages = nil }, false},
		{"root_digest", func(p *receiptPairFixture) { p.a.RootIdentity = "sha256:x" }, false},
		{"selected_count", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.PresentCount++ }) }, true},
		{"absent_count", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.IndexedAbsentCount++ }) }, true},
		{"unknown_absent", func(p *receiptPairFixture) { p.a.UnindexedAbsentCount = 1 }, false},
		{"selected_order", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[0], r.Selected[1] = r.Selected[1], r.Selected[0] })
		}, true},
		{"stage_order", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Stages[0], r.Stages[1] = r.Stages[1], r.Stages[0] })
		}, true},
		{"selected_duplicate", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[1] = r.Selected[0] })
		}, true},
		{"protected_path", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].Path = ".HiNt/private" })
		}, true},
		{"unsafe_mode", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].Mode = "120000" })
		}, true},
		{"negative_size", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].Size = -1 }) }, true},
		{"oversized_file", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].Size = 65 }) }, true},
		{"total_bytes", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) {
				for i := range r.Selected {
					if r.Selected[i].State == SelectedRegularPresent {
						r.Selected[i].Size = 64
					}
				}
			})
		}, true},
		{"unindexed_relation", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].Indexed = false })
		}, true},
		{"invalid_state", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Selected[0].State = "unknown" })
		}, true},
		{"absent_payload", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) {
				for i := range r.Selected {
					if r.Selected[i].State == SelectedAbsent {
						r.Selected[i].Size = 1
					}
				}
			})
		}, true},
		{"unmerged_stage", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.Stages[0].Stage = 1 }) }, true},
		{"stage_symlink", func(p *receiptPairFixture) { bothSelected(p, func(r *SelectedReceipt) { r.Stages[0].Mode = "120000" }) }, true},
		{"stage_hash", func(p *receiptPairFixture) {
			bothSelected(p, func(r *SelectedReceipt) { r.Stages[0].ObjectID = strings.Repeat("a", 64) })
		}, true},
		{"semantic_digest", func(p *receiptPairFixture) { p.a.SelectedInventoryDigest = "sha256:" + strings.Repeat("1", 64) }, false},
		{"tree_digest", func(p *receiptPairFixture) { p.a.TreeDigest = "sha256:" + strings.Repeat("1", 64) }, false},
		{"source_version", func(p *receiptPairFixture) { p.x.Version = "unknown" }, false},
		{"authority", func(p *receiptPairFixture) { bothSource(p, func(r *SourceViewReceipt) { r.Authority = "trusted" }) }, false},
		{"source_state", func(p *receiptPairFixture) { p.x.State = "complete" }, false},
		{"chain_timing", func(p *receiptPairFixture) { p.x.ChainTiming = "observed_before_selection" }, false},
		{"candidate_order", func(p *receiptPairFixture) {
			bothSource(p, func(r *SourceViewReceipt) {
				r.CandidatePaths[0], r.CandidatePaths[1] = r.CandidatePaths[1], r.CandidatePaths[0]
			})
		}, false},
		{"relative_source", func(p *receiptPairFixture) {
			bothSource(p, func(r *SourceViewReceipt) { r.Facts[0].Path = "relative" })
		}, true},
		{"source_fact_order", func(p *receiptPairFixture) {
			bothSource(p, func(r *SourceViewReceipt) { r.Facts[0], r.Facts[1] = r.Facts[1], r.Facts[0] })
		}, true},
		{"source_fact_duplicate", func(p *receiptPairFixture) { bothSource(p, func(r *SourceViewReceipt) { r.Facts[1] = r.Facts[0] }) }, true},
		{"source_unknown_role", func(p *receiptPairFixture) { bothSource(p, func(r *SourceViewReceipt) { r.Facts[0].Role = "unknown" }) }, true},
		{"absent_bytes", func(p *receiptPairFixture) {
			bothSource(p, func(r *SourceViewReceipt) {
				for i := range r.Facts {
					if r.Facts[i].State == "observed_absent" {
						r.Facts[i].Bytes = 1
						break
					}
				}
			})
		}, true},
		{"present_alternates", func(p *receiptPairFixture) {
			bothSource(p, func(r *SourceViewReceipt) {
				for i := range r.Facts {
					if strings.HasSuffix(r.Facts[i].Path, "/objects/info/alternates") {
						f := &r.Facts[i]
						f.State = "present_regular"
						f.ObjectDigest = library.Digest(nil)
						f.NodeIdentity = library.Digest([]byte("synthetic node"))
						f.AbsenceIdentity = ""
					}
				}
			})
		}, true},
		{"metadata_reads", func(p *receiptPairFixture) { bothSource(p, func(r *SourceViewReceipt) { r.MetadataReads++ }) }, false},
		{"metadata_bytes", func(p *receiptPairFixture) { bothSource(p, func(r *SourceViewReceipt) { r.MetadataBytes++ }) }, false},
		{"source_query_count", func(p *receiptPairFixture) { p.x.SourceQueryAttempts = 25 }, false},
		{"dispatch_relation", func(p *receiptPairFixture) { p.x.PersistenceGuardAttempts++ }, false},
		{"executable_reads", func(p *receiptPairFixture) { p.x.ExecutableReads++ }, false},
		{"executable_bytes", func(p *receiptPairFixture) { p.x.ExecutableReadBytes++ }, false},
		{"source_output_cap", func(p *receiptPairFixture) { p.x.SourceStdoutBytes = sourceQueryCap + 1 }, false},
		{"stderr", func(p *receiptPairFixture) { p.x.GuardStderrBytes = 1 }, false},
		{"negative_guard_output", func(p *receiptPairFixture) { p.x.GuardStdoutBytes = -1 }, false},
		{"selected_clock", func(p *receiptPairFixture) { p.a.BeginOffsetNS = -1 }, false},
		{"source_clock", func(p *receiptPairFixture) { p.x.EndOffsetNS = p.a.EndOffsetNS - 1 }, false},
		{"pair_clock_overlap", func(p *receiptPairFixture) {
			p.b.BeginOffsetNS = p.x.EndOffsetNS - 1
			p.y.BeginOffsetNS = p.b.BeginOffsetNS
		}, false},
		{"deadline", func(p *receiptPairFixture) { p.y.EndOffsetNS = int64(strictCaptureSeconds) + 1 }, false},
		{"pair_source_binding", func(p *receiptPairFixture) { p.y.InvocationIdentity = library.Digest([]byte("other profile")) }, false},
		{"pair_node_binding", func(p *receiptPairFixture) { p.b.Selected[0].PathIdentity = library.Digest([]byte("other node")) }, false},
		{"bounded_strings", func(p *receiptPairFixture) { p.x.Facts[0].Path = "/" + strings.Repeat("x", int(receiptStringLimit)) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base.copy()
			tt.mutate(&p)
			if tt.rebind {
				rebindPairDigests(&p)
			}
			if e := p.validate(); e == nil {
				t.Fatal("malformed synthetic receipt pair accepted")
			}
		})
	}
}
func TestSelectedIgnoreChainSharedProjection(t *testing.T) {
	got, e := selectedIgnoreChain("/synthetic/root", []string{"dir/file", "dir/other", "top"}, 0)
	want := []string{"/synthetic/root/.gitignore", "/synthetic/root/dir/.gitignore"}
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("literal ignore projection changed")
	}
	if _, e := selectedIgnoreChain("/synthetic/root", []string{strings.Repeat("a/", 2000) + "leaf"}, 0); e == nil {
		t.Fatal("over-limit projection accepted")
	}
	if _, e := selectedIgnoreChain("/synthetic/root", []string{"dir/file"}, 1); e == nil {
		t.Fatal("pure consumer projection byte bound ignored")
	}
}

func TestSourceViewEmptyChainStillRejectsChangedChain(t *testing.T) {
	v, _ := openValidationView(t, "empty")
	defer func() {
		if e := v.Close(); e != nil {
			t.Error(e)
		}
	}()
	a, e := v.Capture(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(a.Selected().Receipt().Selected) != 0 || v.firstChain == nil || len(v.firstChain) != 0 {
		t.Fatal("actual private empty chain not normalized")
	}
	testkit.WriteFile(t, v.root, "new-dir/new-file", "synthetic new selected source")
	chain, e := selectedIgnoreChain(v.root, []string{"new-dir/new-file"}, 0)
	if e != nil || len(chain) != 2 {
		t.Fatal("changed nonempty chain control")
	}
	if b, e := v.Capture(context.Background()); e == nil || b != nil {
		t.Fatal("empty-to-nonempty source chain admitted")
	}
}

func TestValidateSelectedSourcePairDoesNotAttestRawObjects(t *testing.T) {
	p := capturedValidationPair(t, "mode_untracked")
	// Synthetic absent object refs satisfy the structural pair. No object bytes
	// enter this API; the future factual loader must refuse their missing bytes.
	p.x.ConfigObject = library.Digest([]byte("synthetic missing object"))
	p.y.ConfigObject = p.x.ConfigObject
	if e := p.validate(); e != nil {
		t.Fatal("pure helper pretended to require object IO", e)
	}
}
