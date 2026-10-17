// Package build packages an episode as an eval version: exact base state, instruction,
// hidden verifier, reference solution and a pinned environment.
//
// Safety rules:
//   - No command text from a session is executed. A session's passing check only selects
//     one of the fixed check templates of the detected language.
//   - The agent image holds the base state only: no Git history, no hidden tests, no
//     reference solution, no session text other than the instruction.
//   - The instruction comes from the user's request and corrections; it never quotes the
//     result state.
//   - Anything that cannot be built exactly is refused with a reason.
package build

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/trees"
)

// Image is a pinned base image (manifest list digest, unchanged for at least two weeks when
// it was pinned).
type Image struct {
	Ref     string // name:tag@sha256:...
	Version string // language version
}

// Images are the pinned images per language, newest first.
var Images = map[string][]Image{
	"go": {
		{"golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452", "1.27"},
		{"golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514", "1.26"},
		{"golang:1.25.14-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437", "1.25"},
	},
	"python": {
		{"python:3.14.7-bookworm@sha256:bfb689a7986adc6d5f16722e06c78e755efe4062e56276715fef450cbad09436", "3.14"},
		{"python:3.13.15-bookworm@sha256:227b6570d6ee07061ae6ca2eb04dedfb6d2b34045835f343065b9869e4d427ea", "3.13"},
		{"python:3.12.14-bookworm@sha256:dbbe4ceb97851e2e5fa83798b239811f871cb743b259ba3563737349f6bcfaa0", "3.12"},
	},
}

// Workdir is where the repository lives in the task image.
const Workdir = "/app"

// Refusal explains why an episode cannot be packaged. Reasons are eval reason codes.
type Refusal struct{ Reasons []string }

func (r *Refusal) Error() string { return "not packaged: " + strings.Join(r.Reasons, ", ") }

// Input is everything the builder reads; it holds texts that stay local.
type Input struct {
	EpisodeID   string
	Family      string
	Repo        string
	Base        string   // HINT tree digest
	Result      string   // HINT tree digest
	Paths       []string // files the episode edited (relative)
	Attribution string
	Outcome     string
	Request     string // request text
	RequestAt   map[string]any
	Corrections []Text
	CheckHint   string // the session's passing check command (selects a template only)
	Now         time.Time
}

// Text is a request or correction with its source location.
type Text struct {
	Body string
	At   map[string]any
}

// Built is the packaged eval plus the object digests of its parts.
type Built struct {
	Record   map[string]any // eval record (schema eval), state packaged
	Language string
	Hidden   []string // hidden test paths
	Solution []string // changed source paths
	Deleted  []string
}

var testFile = map[string]*regexp.Regexp{
	"go":     regexp.MustCompile(`(^|/)[^/]+_test\.go$`),
	"python": regexp.MustCompile(`(^|/)(test_[^/]+\.py|[^/]+_test\.py|conftest\.py)$|(^|/)tests?/[^/]+\.py$`),
}

// Language detects the language of a tree from its manifest files.
func Language(t trees.Tree) string {
	has := map[string]bool{}
	for _, f := range t.Files {
		has[f.Path] = true
	}
	switch {
	case has["go.mod"]:
		return "go"
	case has["pyproject.toml"] || has["setup.py"] || has["requirements.txt"] || has["setup.cfg"]:
		return "python"
	}
	return ""
}

var goDirective = regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+)`)

func pickImage(l *library.Library, lang string, t trees.Tree) (Image, error) {
	imgs := Images[lang]
	if len(imgs) == 0 {
		return Image{}, &Refusal{[]string{"external_service"}}
	}
	if lang == "go" {
		for _, f := range t.Files {
			if f.Path == "go.mod" {
				b, err := l.GetObject(f.Digest)
				if err != nil {
					return Image{}, err
				}
				if m := goDirective.FindSubmatch(b); m != nil {
					want := string(m[1])
					for i := len(imgs) - 1; i >= 0; i-- { // the oldest pinned image that is new enough
						if versionGE(imgs[i].Version, want) {
							return imgs[i], nil
						}
					}
					return Image{}, &Refusal{[]string{"external_service"}} // no pinned toolchain is new enough
				}
			}
		}
	}
	return imgs[0], nil
}

func versionGE(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 2; i++ {
		var x, y int
		fmt.Sscan(pa[i], &x)
		if i < len(pb) {
			fmt.Sscan(pb[i], &y)
		}
		if x != y {
			return x > y
		}
	}
	return true
}

var checkTemplate = map[string]*regexp.Regexp{
	"go":     regexp.MustCompile(`(^|[\s;&|(])go test\b`),
	"python": regexp.MustCompile(`(^|[\s;&|(])(pytest|python3? -m pytest)\b`),
}

// Build packages one episode. It stores every part in the library and returns the eval
// record (state packaged), or a Refusal.
func Build(l *library.Library, in Input) (*Built, error) {
	var reasons []string
	if in.Attribution == "ambiguous" {
		reasons = append(reasons, "attribution_ambiguous")
	}
	if in.Base == "" {
		reasons = append(reasons, "baseline_missing")
	}
	if in.Result == "" {
		reasons = append(reasons, "result_state_missing")
	}
	if strings.TrimSpace(in.Request) == "" {
		reasons = append(reasons, "requirements_missing")
	}
	if len(reasons) > 0 {
		return nil, &Refusal{reasons}
	}
	base, err := trees.Load(l, in.Base)
	if err != nil {
		return nil, &Refusal{[]string{"baseline_missing"}}
	}
	result, err := trees.Load(l, in.Result)
	if err != nil {
		return nil, &Refusal{[]string{"result_state_missing"}}
	}
	lang := Language(base)
	if lang == "" {
		return nil, &Refusal{[]string{"verifier_missing"}}
	}
	if in.CheckHint == "" || !checkTemplate[lang].MatchString(in.CheckHint) || in.Outcome != "test_passed" {
		return nil, &Refusal{[]string{"verifier_missing"}}
	}
	img, err := pickImage(l, lang, base)
	if err != nil {
		return nil, err
	}
	// the episode's change: only files the episode edited, plus test files it added (the
	// rest of the diff is not attributed to the episode)
	added, removed, changed := trees.Diff(base, result)
	edited := map[string]bool{}
	for _, p := range in.Paths {
		edited[p] = true
	}
	isTest := testFile[lang]
	resFiles := map[string]trees.File{}
	for _, f := range result.Files {
		resFiles[f.Path] = f
	}
	var hidden, solution, deleted []string
	unattributed := 0
	for _, p := range append(append([]string{}, added...), changed...) {
		switch {
		case isTest.MatchString(p) && (edited[p] || contains(added, p)):
			hidden = append(hidden, p)
		case edited[p]:
			solution = append(solution, p)
		default:
			unattributed++
		}
	}
	for _, p := range removed {
		if edited[p] {
			deleted = append(deleted, p)
		} else {
			unattributed++
		}
	}
	if len(hidden) == 0 {
		return nil, &Refusal{[]string{"verifier_missing"}}
	}
	if len(solution)+len(deleted) == 0 {
		return nil, &Refusal{[]string{"no_checkable_outcome"}}
	}
	// agent image: base tree as one archive (no history); hidden test paths that exist in
	// the base keep their base version (visible), the result version goes to the verifier
	baseArchive, err := tarTree(l, base.Files, "")
	if err != nil {
		return nil, err
	}
	baseArc, err := l.PutObject(baseArchive)
	if err != nil {
		return nil, err
	}
	checks := checkScript(lang, hidden)
	vfiles := []trees.File{}
	for _, p := range hidden {
		vfiles = append(vfiles, resFiles[p])
	}
	vArchive, err := tarTree(l, vfiles, "hidden/", extra{"test.sh", []byte(checks), 0o755}, extra{"checks.json", checksJSON(lang, hidden), 0o644})
	if err != nil {
		return nil, err
	}
	vArc, err := l.PutObject(vArchive)
	if err != nil {
		return nil, err
	}
	sfiles := []trees.File{}
	for _, p := range solution {
		sfiles = append(sfiles, resFiles[p])
	}
	sArchive, err := tarTree(l, sfiles, "files/", extra{"solve.sh", []byte(solveScript), 0o755}, extra{"deleted.txt", []byte(strings.Join(deleted, "\n")), 0o644})
	if err != nil {
		return nil, err
	}
	sArc, err := l.PutObject(sArchive)
	if err != nil {
		return nil, err
	}
	instr := instruction(in, lang)
	instrD, err := l.PutObject([]byte(instr))
	if err != nil {
		return nil, err
	}
	setup := setupScript(lang)
	setupD, err := l.PutObject([]byte(setup))
	if err != nil {
		return nil, err
	}
	reqText, _ := l.PutObject([]byte(in.Request))
	reqs := []any{map[string]any{"req_id": "R1", "kind": "behavior", "origin": "request", "text": reqText, "evidence": []any{in.RequestAt}, "checks": []any{"C1"}}}
	for i, c := range in.Corrections {
		d, _ := l.PutObject([]byte(c.Body))
		reqs = append(reqs, map[string]any{"req_id": fmt.Sprintf("R%d", i+2), "kind": "behavior", "origin": "correction", "text": d, "evidence": []any{c.At}, "checks": []any{"C1"}})
	}
	preserve, _ := l.PutObject([]byte("The existing tests keep passing."))
	reqs = append(reqs, map[string]any{"req_id": fmt.Sprintf("R%d", len(reqs)+1), "kind": "preserve", "origin": "existing_test", "text": preserve, "evidence": []any{}, "checks": []any{"C2"}})
	evalID := "ev-" + short(in.EpisodeID, in.Base, in.Result)
	rec := map[string]any{
		"record": "eval", "manifest_version": 1, "eval_id": evalID, "version": 1, "previous": nil,
		"created_at": in.Now.UTC().Format(time.RFC3339), "episode_id": in.EpisodeID, "family": orDefault(in.Family, "bug_fix"),
		"repositories": []any{map[string]any{"repo": in.Repo, "base_tree": in.Base, "base_archive": baseArc, "result_tree": in.Result}},
		"instruction":  instrD, "requirements": reqs,
		"verifier": map[string]any{"archive": vArc, "checks": []any{
			map[string]any{"check_id": "C1", "origin": "result_test"}, map[string]any{"check_id": "C2", "origin": "existing_test"}}},
		"reference":    sArc,
		"environment":  map[string]any{"image": img.Ref, "setup": setupD, "workdir": Workdir, "test_command": "tests/test.sh"},
		"dependencies": []any{}, "state": "packaged", "reasons": reasonsFor(unattributed),
		"qualification": map[string]any{"policy": "q1", "checks": []any{}}, "override": nil,
	}
	return &Built{Record: rec, Language: lang, Hidden: hidden, Solution: solution, Deleted: deleted}, nil
}

func reasonsFor(unattributed int) []any {
	if unattributed > 0 {
		return []any{"unrelated_edits"} // left out of the reference; qualification decides
	}
	return []any{}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func short(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

func instruction(in Input, lang string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(in.Request))
	b.WriteString("\n")
	if len(in.Corrections) > 0 {
		b.WriteString("\nLater clarifications from the same request:\n")
		for _, c := range in.Corrections {
			b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(c.Body), "\n", " ") + "\n")
		}
	}
	b.WriteString("\nThe repository is in " + Workdir + ". Change the code so that the request is met; keep the existing tests passing.\n")
	return b.String()
}

// checkScript runs the fixed checks: C1 the hidden tests (copied over the work tree), C2
// the full existing suite. It prints one line per check and writes the reward; it runs
// in the separate verifier container with no network.
func checkScript(lang string, hidden []string) string {
	var c1, c2 string
	switch lang {
	case "go":
		pk := map[string]bool{}
		for _, p := range hidden {
			pk["./"+path.Dir(p)] = true
		}
		pkgs := keys(pk)
		c1 = "go test -count=1 " + strings.Join(pkgs, " ")
		c2 = "go test -count=1 ./..."
	case "python":
		c1 = "python -m pytest -q -p no:cacheprovider " + strings.Join(quoteAll(hidden), " ")
		c2 = "python -m pytest -q -p no:cacheprovider"
	}
	return `#!/bin/sh
# Generated verifier: fixed checks, no session command. Writes /logs/verifier/reward.txt.
set -u
mkdir -p /logs/verifier
reward=0
trap 'echo "$reward" > /logs/verifier/reward.txt' EXIT
cd ` + Workdir + ` || exit 0
cp -R /tests/hidden/. ` + Workdir + `/ || exit 0
ok=1
if ` + c1 + ` >/logs/verifier/c1.txt 2>&1; then echo "CHECK C1 PASS"; else echo "CHECK C1 FAIL"; ok=0; fi
if ` + c2 + ` >/logs/verifier/c2.txt 2>&1; then echo "CHECK C2 PASS"; else echo "CHECK C2 FAIL"; ok=0; fi
[ "$ok" = 1 ] && reward=1
exit 0
`
}

func checksJSON(lang string, hidden []string) []byte {
	b, _ := json.Marshal(map[string]any{"language": lang, "checks": []string{"C1", "C2"}, "hidden": hidden})
	return b
}

const solveScript = `#!/bin/sh
# Reference solution: copy the changed files, remove deleted ones.
set -eu
cp -R /solution/files/. /app/
if [ -s /solution/deleted.txt ]; then while IFS= read -r p; do rm -f "/app/$p"; done < /solution/deleted.txt; fi
`

func setupScript(lang string) string {
	switch lang {
	case "go":
		return "#!/bin/sh\nset -eu\ncd /app\n[ -f go.mod ] && go mod download\n"
	case "python":
		return "#!/bin/sh\nset -eu\ncd /app\npython -m pip install -q pytest==8.4.2\n[ -f requirements.txt ] && python -m pip install -q -r requirements.txt || true\n"
	}
	return "#!/bin/sh\n"
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var safeArg = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

func quoteAll(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if safeArg.MatchString(p) {
			out = append(out, p)
		} else {
			out = append(out, "'"+strings.ReplaceAll(p, "'", `'\''`)+"'")
		}
	}
	return out
}

type extra struct {
	name string
	data []byte
	mode int64
}

// tarTree writes a deterministic tar: sorted entries, fixed times and owners.
func tarTree(l *library.Library, files []trees.File, prefix string, extras ...extra) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	epoch := time.Unix(0, 0)
	for _, e := range extras {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), ModTime: epoch, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		tw.Write(e.data)
	}
	for _, f := range files {
		data, err := l.GetObject(f.Digest)
		if err != nil {
			return nil, err
		}
		h := &tar.Header{Name: prefix + f.Path, ModTime: epoch, Format: tar.FormatPAX}
		switch f.Mode {
		case "120000":
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, string(data), 0o777
		default:
			h.Typeflag, h.Size, h.Mode = tar.TypeReg, int64(len(data)), 0o644
			if f.Mode == "100755" {
				h.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(data)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
