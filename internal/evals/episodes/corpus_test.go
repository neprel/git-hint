package episodes

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/sessions"
	"github.com/neprel/git-hint/internal/store"
	"github.com/neprel/git-hint/internal/testkit"
)

type corpusCase struct {
	Now       int64                        `json:"now"`
	Snapshots map[string]map[string]string `json:"snapshots"`
	Sessions  []map[string]any             `json:"sessions"`
}

type expEpisode struct {
	Session     string           `json:"session"`
	Requests    []string         `json:"requests"`
	Corrections []string         `json:"corrections"`
	Base        *string          `json:"base"`
	Result      *string          `json:"result"`
	Paths       []string         `json:"paths"`
	Attribution string           `json:"attribution"`
	Outcome     string           `json:"outcome"`
	Family      *string          `json:"family"`
	Links       []map[string]any `json:"links"`
	Reasons     []string         `json:"reasons"`
	Children    *int             `json:"child_sessions"`
}

func snapID(name string) string {
	h := sha1.Sum([]byte("snap:" + name))
	return hex.EncodeToString(h[:])
}

func lib(t *testing.T) *library.Library {
	t.Helper()
	t.Setenv("HINT_KEY_DIR", t.TempDir())
	dir := t.TempDir()
	testkit.Git(t, dir, "init", "-q")
	s, _, err := store.Init(dir, store.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return library.Open(s, 0)
}

// runCase imports the case sessions and discovers episodes; it returns the proposals and
// the tree digest -> snapshot name map.
func runCase(t *testing.T, dir string, seg ...Segmenter) ([]*Proposal, map[string]string, []expEpisode) {
	t.Helper()
	var c corpusCase
	b, _ := os.ReadFile(filepath.Join(dir, "case.json"))
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Episodes []expEpisode `json:"episodes"`
	}
	b, _ = os.ReadFile(filepath.Join(dir, "expected.json"))
	json.Unmarshal(b, &exp)
	l := lib(t)
	for _, sess := range c.Sessions {
		parent, _ := sess["_parent"].(string)
		delete(sess, "_parent")
		raw, _ := json.Marshal(sess)
		src := []byte(strings.NewReplacer().Replace(string(raw)))
		for name := range c.Snapshots {
			src = []byte(strings.ReplaceAll(string(src), `"SNAP:`+name+`"`, `"`+snapID(name)+`"`))
		}
		src = []byte(strings.ReplaceAll(string(src), `"SNAP:gone"`, `"`+snapID("gone")+`"`))
		r, err := sessions.ParseOpenCode(src, sessions.OpenCodeOptions{Version: "2.0.26", Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.Import(l, src, r); err != nil {
			t.Fatal(err)
		}
	}
	trees := map[string]string{} // agent snapshot id -> fake tree digest
	names := map[string]string{} // fake tree digest -> snapshot name
	for name := range c.Snapshots {
		d := library.Digest([]byte("tree:" + name))
		trees[snapID(name)], names[d] = d, name
	}
	var sg Segmenter
	if len(seg) > 0 {
		sg = seg[0]
	}
	props, err := DiscoverWith(l, time.UnixMilli(c.Now), func(_, id string) string { return trees[id] },
		func(dir string) string {
			if dir == "/work/shop" {
				return "shop"
			}
			return ""
		}, sg)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range props {
		if err := evals.ValidateValue("episode", p.Record); err != nil {
			t.Fatalf("%s: invalid episode: %v", dir, err)
		}
	}
	if _, _, err := Store(l, props); err != nil {
		t.Fatal(err)
	}
	return props, names, exp.Episodes
}

type actual struct {
	Corrections []string
	Base        *string
	Result      *string
	Paths       []string
	Attribution string
	Outcome     string
	Family      *string
	Links       []string
	Reasons     []string
	Children    int
}

func actualOf(p *Proposal, names map[string]string, byID map[string]*Proposal) actual {
	a := actual{Paths: p.Paths, Attribution: p.Record["attribution"].(string), Outcome: p.Outcome}
	a.Corrections = p.Requests[1:]
	repo := p.Record["repositories"].([]any)[0].(map[string]any)
	if d, ok := repo["base_tree"].(string); ok {
		n := names[d]
		a.Base = &n
	}
	if d, ok := repo["result_tree"].(string); ok {
		n := names[d]
		a.Result = &n
	}
	if f, ok := p.Record["family"].(string); ok {
		a.Family = &f
	}
	for _, l := range p.Record["links"].([]any) {
		m := l.(map[string]any)
		a.Links = append(a.Links, fmt.Sprintf("%s:%s:%s", m["kind"], byID[m["episode_id"].(string)].Requests[0], m["status"]))
	}
	a.Reasons = append([]string{}, p.Record["reasons"].([]string)...)
	sort.Strings(a.Reasons)
	a.Children = p.Record["usage"].(map[string]any)["child_sessions"].(int)
	return a
}

func expectedOf(e expEpisode) actual {
	a := actual{Corrections: e.Corrections, Base: e.Base, Result: e.Result, Paths: e.Paths, Attribution: e.Attribution, Outcome: e.Outcome, Family: e.Family}
	for _, l := range e.Links {
		a.Links = append(a.Links, fmt.Sprintf("%s:%s:%s", l["kind"], l["on_request"], l["status"]))
	}
	a.Reasons = append([]string{}, e.Reasons...)
	sort.Strings(a.Reasons)
	if e.Children != nil {
		a.Children = *e.Children
	}
	return a
}

type tally struct {
	expected, predicted, matched, exact int
	fields                              map[string][2]int // field -> [correct, total]
	safety                              []string
}

func score(t *testing.T, split string, seg ...Segmenter) tally {
	dirs, _ := filepath.Glob(filepath.Join("..", "..", "..", "tests", "evals", "corpus", split, "*"))
	if len(dirs) == 0 {
		t.Fatal("no corpus cases")
	}
	tl := tally{fields: map[string][2]int{}}
	for _, dir := range dirs {
		props, names, exp := runCase(t, dir, seg...)
		byID := map[string]*Proposal{}
		got := map[string]*Proposal{}
		for _, p := range props {
			byID[p.Record["episode_id"].(string)] = p
			got[p.Session+"/"+p.Requests[0]] = p
		}
		tl.expected += len(exp)
		tl.predicted += len(props)
		for _, e := range exp {
			p, ok := got[e.Session+"/"+e.Requests[0]]
			if !ok {
				continue
			}
			tl.matched++
			a, w := actualOf(p, names, byID), expectedOf(e)
			av, wv := reflect.ValueOf(a), reflect.ValueOf(w)
			all := true
			for i := 0; i < av.NumField(); i++ {
				name := av.Type().Field(i).Name
				eq := reflect.DeepEqual(norm(av.Field(i).Interface()), norm(wv.Field(i).Interface()))
				c := tl.fields[name]
				c[1]++
				if eq {
					c[0]++
				} else {
					all = false
					t.Logf("%s/%s %s: got %v want %v", split, filepath.Base(dir), name, show(av.Field(i).Interface()), show(wv.Field(i).Interface()))
				}
				tl.fields[name] = c
			}
			if all {
				tl.exact++
			}
			// time-overlap ambiguity must never become confirmed ownership; an interleaving
			// that only a model can see (expected reason model_segmented) is a quality
			// miss when no model or a wrong model answers, not a confirmation
			if w.Attribution == "ambiguous" && a.Attribution != "ambiguous" && !contains(e.Reasons, "model_segmented") {
				tl.safety = append(tl.safety, filepath.Base(dir)+": ambiguous ownership confirmed")
			}
			if w.Base == nil && a.Base != nil {
				tl.safety = append(tl.safety, filepath.Base(dir)+": fabricated baseline")
			}
		}
		requests := map[string]int{}
		for _, p := range props {
			if strings.HasPrefix(p.Record["episode_id"].(string), "ses_") {
				tl.safety = append(tl.safety, "episode ID takes a session form")
			}
			for _, r := range p.Requests {
				requests[p.Session+"/"+r]++
			}
			if hasReason(p, "model_segmented") && p.Record["boundary"].(map[string]any)["basis"] != "inferred" {
				tl.safety = append(tl.safety, filepath.Base(dir)+": a model boundary is not inferred")
			}
		}
		for r, n := range requests {
			if n != 1 {
				tl.safety = append(tl.safety, fmt.Sprintf("%s: request %s in %d episodes", filepath.Base(dir), r, n))
			}
		}
	}
	return tl
}

func norm(v any) any {
	switch x := v.(type) {
	case []string:
		if len(x) == 0 {
			return []string(nil)
		}
	case *string:
		if x == nil {
			return "<nil>"
		}
		return *x
	}
	return v
}

func show(v any) any { return norm(v) }

func report(t *testing.T, split string, tl tally) {
	t.Logf("%s: expected %d predicted %d matched %d exact %d; precision %.2f recall %.2f", split, tl.expected, tl.predicted, tl.matched, tl.exact,
		ratio(tl.matched, tl.predicted), ratio(tl.matched, tl.expected))
	keys := make([]string, 0, len(tl.fields))
	for k := range tl.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := tl.fields[k]
		t.Logf("  %-12s %d/%d", k, c[0], c[1])
	}
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

// Development cases must match exactly.
func TestCorpusDev(t *testing.T) {
	tl := score(t, "dev")
	report(t, "dev", tl)
	if tl.exact != tl.expected || tl.predicted != tl.expected || len(tl.safety) > 0 {
		t.Fatalf("dev corpus: %+v", tl)
	}
}

// Held-out cases measure quality; the safety properties are hard requirements.
func TestCorpusHeldOut(t *testing.T) {
	tl := score(t, "heldout")
	report(t, "heldout", tl)
	if len(tl.safety) > 0 {
		t.Fatalf("held-out safety: %v", tl.safety)
	}
}

// correctSegmenter answers the held-out-seg cases as a correct model would.
func correctSegmenter() Segmenter {
	return func(session string, reqs []string) ([]string, map[string]string, map[string]any, bool) {
		gen := map[string]any{"name": "segment", "version": "test", "inputs": []any{}, "output": library.Digest([]byte(session)), "requests": 1}
		switch session {
		case "ses_r":
			return []string{"A", "B"}, map[string]string{"A": "bug_fix", "B": "feature"}, gen, true
		case "ses_s":
			return []string{"A"}, map[string]string{"A": "bug_fix"}, gen, true
		case "ses_t":
			return []string{"A", "A"}, map[string]string{"A": "bug_fix"}, gen, true
		case "ses_u":
			return []string{"A", "B", "A"}, map[string]string{"A": "bug_fix", "B": "feature"}, gen, true
		}
		return nil, nil, nil, false
	}
}

// E06 model segmentation on the frozen held-out-seg cases: the rules alone miss the
// split, the unclassified family and the interleaving; with a correct segmentation every
// case matches. The safety checks (every request once, ambiguous stays ambiguous, model
// boundaries inferred) hold in both runs. The live model's quality is a separate,
// permission-bound measurement.
func TestCorpusModelSegmentation(t *testing.T) {
	rules := score(t, "heldout-seg")
	report(t, "heldout-seg rules only", rules)
	with := score(t, "heldout-seg", correctSegmenter())
	report(t, "heldout-seg with a correct segmentation", with)
	if len(rules.safety) > 0 || len(with.safety) > 0 {
		t.Fatalf("safety: %v %v", rules.safety, with.safety)
	}
	if with.exact != with.expected || with.predicted != with.expected || rules.exact == rules.expected {
		t.Fatalf("rules %+v with %+v", rules, with)
	}
	// the other splits do not change when a segmenter answers nothing
	none := func(string, []string) ([]string, map[string]string, map[string]any, bool) {
		return nil, nil, nil, false
	}
	if tl := score(t, "dev", none); tl.exact != tl.expected {
		t.Fatalf("dev with a silent segmenter: %+v", tl)
	}
}

// A wrong or hostile segmentation cannot drop requests, invent completion or confirm an
// interleaving: every request stays once, a part without its own passing check has no
// passed outcome, and an answer of the wrong size changes nothing.
func TestSegmentationSafety(t *testing.T) {
	everySplit := func(_ string, reqs []string) ([]string, map[string]string, map[string]any, bool) {
		out := make([]string, len(reqs))
		for i := range reqs {
			out[i] = fmt.Sprint("T", i)
		}
		return out, map[string]string{"T0": "refactor"}, map[string]any{"name": "segment", "version": "test", "inputs": []any{}, "output": library.Digest([]byte("x")), "requests": 1}, true
	}
	for _, split := range []string{"dev", "heldout", "heldout-seg"} {
		tl := score(t, split, everySplit)
		if len(tl.safety) > 0 {
			t.Fatalf("%s: %v", split, tl.safety)
		}
	}
	props, _, _ := runCase(t, filepath.Join("..", "..", "..", "tests", "evals", "corpus", "heldout-seg", "two-tasks-no-check-between"), everySplit)
	if len(props) != 2 || props[0].Outcome != "none" || props[1].Outcome != "test_passed" {
		t.Fatalf("outcomes %v %v", props[0].Outcome, props[1].Outcome)
	}
	wrongSize := func(_ string, reqs []string) ([]string, map[string]string, map[string]any, bool) {
		return []string{"A"}, nil, nil, true
	}
	props, _, _ = runCase(t, filepath.Join("..", "..", "..", "tests", "evals", "corpus", "heldout-seg", "two-tasks-no-check-between"), wrongSize)
	if len(props) != 1 || hasReason(props[0], "model_segmented") {
		t.Fatalf("a wrong-size answer changed the episode: %d", len(props))
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
