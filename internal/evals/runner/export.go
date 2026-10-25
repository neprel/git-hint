// Package runner exports eval versions as task directories and runs them in HINT's built-in
// container runner (docker.go) with fixed safety settings.
//
// The HINT manifest stays authoritative: the task directory is generated from it, and its
// round trip is checked by digests. Settings that HINT always applies:
//   - separate verifier container (in a shared container the agent could write the reward);
//   - no network for the task container at run time (a live agent reaches only HINT's model
//     gate; image builds still have network for pinned setup);
//   - process limit and no-new-privileges.
package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals/iodecl"
	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/evals/safefs"
)

// Settings are the limits written into a task.
type Settings struct {
	AgentSeconds    int
	VerifierSeconds int
	BuildSeconds    int
	CPUs            float64
	MemoryMB        int
	Pids            int
}

// DefaultSettings are finite limits for one trial.
var DefaultSettings = Settings{AgentSeconds: 1800, VerifierSeconds: 600, BuildSeconds: 900, CPUs: 2, MemoryMB: 2048, Pids: 512}

// Task is an exported task directory with the digests of its inputs.
type Task struct {
	Dir    string
	Name   string
	Inputs map[string]string // part -> digest of the source object
	Canary string
}

// Export writes the task directory of one eval version into dir (empty or absent).
func Export(l *library.Library, rec map[string]any, dir string, s Settings) (*Task, error) {
	return exportTask(l, rec, dir, s, nil)
}

// ExportBounded shares the exact exporter. It bounds original source-object
// reads only. Generated private-directory rereads retain the existing cooperative
// context/no-outside-writer assumption; this is not an adversarial whole-IO bound.
func ExportBounded(parent context.Context, l *library.Library, rec map[string]any, dir string, s Settings) (*Task, error) {
	if parent == nil || l == nil || parent.Err() != nil {
		return nil, errors.New("bounded_export_context")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	b := &exportObjectBudget{ctx: ctx}
	task, e := exportTask(l, rec, dir, s, b)
	if e != nil || ctx.Err() != nil {
		return nil, errors.New("bounded_export_refused")
	}
	return task, nil
}

type exportObjectBudget struct {
	ctx   context.Context
	used  int64
	parts int
}

func (b *exportObjectBudget) read(l *library.Library, part, digest string) ([]byte, error) {
	cap := int64(32 << 20)
	switch part {
	case "instruction", "setup":
		cap = 1 << 20
	case "base_archive", "verifier", "reference":
	default:
		return nil, errors.New("bounded_export_part")
	}
	if b.ctx.Err() != nil || b.parts >= 5 || b.used >= 128<<20 {
		return nil, errors.New("bounded_export_limit")
	}
	b.parts++
	if left := int64(128<<20) - b.used; left < cap {
		cap = left
	}
	raw, e := l.ResolveBoundedRead(b.ctx, "object", digest, cap)
	if e != nil {
		return nil, errors.New("bounded_export_source")
	}
	b.used += int64(len(raw))
	return raw, nil
}
func exportTask(l *library.Library, rec map[string]any, dir string, s Settings, budget *exportObjectBudget) (*Task, error) {
	if budget != nil && budget.ctx.Err() != nil {
		return nil, errors.New("bounded_export_context")
	}

	if s.AgentSeconds <= 0 {
		s = DefaultSettings
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		return nil, errors.New("export needs an empty directory")
	}
	evalID, _ := rec["eval_id"].(string)
	version := int(num(rec["version"]))
	env, _ := rec["environment"].(map[string]any)
	image, _ := env["image"].(string)
	repos, _ := rec["repositories"].([]any)
	if evalID == "" || version < 1 || image == "" || len(repos) != 1 {
		return nil, errors.New("eval record is not exportable")
	}
	repo := repos[0].(map[string]any)
	t := &Task{Dir: dir, Name: fmt.Sprintf("hint/%s-v%d", evalID, version), Inputs: map[string]string{}}
	t.Canary = "hint-canary-" + sum([]byte(evalID + "@" + fmt.Sprint(version)))[:32]
	get := func(part, digest string) ([]byte, error) {
		t.Inputs[part] = digest
		if budget != nil {
			return budget.read(l, part, digest)
		}
		return l.GetObject(digest)
	}
	instr, err := get("instruction", str(rec["instruction"]))
	if err != nil {
		return nil, err
	}
	base, err := get("base_archive", str(repo["base_archive"]))
	if err != nil {
		return nil, err
	}
	verifier, err := get("verifier", str(rec["verifier"].(map[string]any)["archive"]))
	if err != nil {
		return nil, err
	}
	var setup []byte
	if d := str(env["setup"]); d != "" {
		if setup, err = get("setup", d); err != nil {
			return nil, err
		}
	}
	w := func(rel string, data []byte, mode os.FileMode) error {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, data, mode)
	}
	// instruction: the canary comment first
	if err := w("instruction.md", []byte("<!-- canary "+t.Canary+" -->\n"+string(instr)), 0o644); err != nil {
		return nil, err
	}
	if err := w("task.toml", []byte(taskTOML(t.Name, version, evalID, s)), 0o644); err != nil {
		return nil, err
	}
	if err := untar(base, filepath.Join(dir, "environment", "repo")); err != nil {
		return nil, err
	}
	if err := w("environment/setup.sh", setup, 0o755); err != nil {
		return nil, err
	}
	if err := w("environment/Dockerfile", []byte(agentDockerfile(image)), 0o644); err != nil {
		return nil, err
	}
	if err := w("environment/docker-compose.yaml", []byte(compose(s)), 0o644); err != nil {
		return nil, err
	}
	if err := untar(verifier, filepath.Join(dir, "tests")); err != nil {
		return nil, err
	}
	// the verifier image needs the module manifest to prepare dependencies offline
	for _, f := range []string{"go.mod", "go.sum", "requirements.txt"} {
		if b, err := os.ReadFile(filepath.Join(dir, "environment", "repo", f)); err == nil {
			if err := w("tests/deps/"+f, b, 0o644); err != nil {
				return nil, err
			}
		}
	}
	if err := w("tests/Dockerfile", []byte(verifierDockerfile(image)), 0o644); err != nil {
		return nil, err
	}
	// the verifier kind must agree with the archive (07B-J §17.2): a cli-file-v1 eval holds a
	// B1 declaration and no test script; any other eval holds no declaration
	kind, _ := rec["verifier"].(map[string]any)["kind"].(string)
	_, specErr := os.Stat(filepath.Join(dir, "tests", "io", "spec.json"))
	_, scriptErr := os.Stat(filepath.Join(dir, "tests", "test.sh"))
	switch {
	case kind == IOInterface && (specErr != nil || scriptErr == nil):
		return nil, errors.New("a cli-file-v1 eval without exactly its B1 declaration")
	case kind != IOInterface && specErr == nil:
		return nil, errors.New("a B1 declaration in an eval of another verifier kind")
	}
	if ref := str(rec["reference"]); ref != "" {
		sol, err := get("reference", ref)
		if err != nil {
			return nil, err
		}
		if err := untar(sol, filepath.Join(dir, "solution")); err != nil {
			return nil, err
		}
		if kind == IOInterface { // the stored reference on the verifier side (never in the agent image)
			if err := untar(sol, filepath.Join(dir, "tests", "io", "reference")); err != nil {
				return nil, err
			}
		}
		if kind == IOInterface {
			// the record's B1 tests are exactly the declared cases, in order (C7BJ-8)
			iot, err := readIOTask(dir)
			if err != nil {
				return nil, err
			}
			want := iodecl.Tests(iot.spec.Cases)
			cs, _ := rec["verifier"].(map[string]any)["checks"].([]any)
			var got []string
			if len(cs) == 1 {
				ts, _ := cs[0].(map[string]any)["tests"].([]any)
				for _, x := range ts {
					s, _ := x.(string)
					got = append(got, s)
				}
			}
			if !slices.Equal(got, want) {
				return nil, errors.New("the B1 tests of the eval are not exactly its declared cases")
			}
		}
	} else if kind == IOInterface {
		return nil, errors.New("a cli-file-v1 eval without a stored reference")
	}
	if kind == IOInterface {
		// the expected agent context, as exported: the digest of every file the agent can see
		// (environment/ and the instruction), kept on the verifier side; the leak probe
		// compares against it instead of guessing from content (C7BJ-9)
		m, err := contextManifest(dir)
		if err != nil {
			return nil, err
		}
		if err := w("tests/io/context.sha256", []byte(m), 0o644); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// contextManifest lists "<sha256> <path>" for every regular file of the agent context of a
// task directory: environment/** and instruction.md, sorted.
func contextManifest(dir string) (string, error) {
	var lines []string
	add := func(rel string) error {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		lines = append(lines, hex.EncodeToString(sum[:])+" "+rel)
		return nil
	}
	if err := add("instruction.md"); err != nil {
		return "", err
	}
	env := filepath.Join(dir, "environment")
	err := filepath.Walk(env, func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		return add(filepath.ToSlash(rel))
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", err
}

func taskTOML(name string, version int, evalID string, s Settings) string {
	// the verifier runs the agent's code with the hidden tests: it gets no network (its image
	// build keeps network for dependency setup)
	agentNet, verifierNet := "", "network_mode = \"no-network\"\n"
	return fmt.Sprintf(`schema_version = "1.4"
artifacts = ["/app"]

[task]
name = %q
version = "%d.0.0"
description = "HINT local eval"

[metadata]
hint_eval = %q
visibility = "private"

[verifier]
timeout_sec = %d.0
environment_mode = "separate"
%s
[agent]
timeout_sec = %d.0
%s
[environment]
network_mode = "public"
build_timeout_sec = %d.0
cpus = %g
memory_mb = %d
storage_mb = 4096
`, name, version, evalID, s.VerifierSeconds, verifierNet, s.AgentSeconds, agentNet, s.BuildSeconds, s.CPUs, s.MemoryMB)
}

// The task container gets no network at run time (network_mode "none").
func compose(s Settings) string {
	return fmt.Sprintf(`services:
  main:
    network_mode: "none"
    pids_limit: %d
    security_opt:
      - "no-new-privileges:true"
`, s.Pids)
}

func agentDockerfile(image string) string {
	return `FROM ` + image + `
WORKDIR /app
COPY repo/ /app/
COPY setup.sh /opt/hint-setup.sh
RUN sh /opt/hint-setup.sh && rm /opt/hint-setup.sh
RUN git init -q && git add -A && git -c user.name=task -c user.email=task@example.invalid commit -qm init
`
}

func verifierDockerfile(image string) string {
	return `FROM ` + image + `
COPY . /tests
RUN if [ -f /tests/deps/go.mod ]; then cd /tests/deps && go mod download; fi
RUN if command -v python >/dev/null && ! command -v go >/dev/null; then python -m pip install -q pytest==8.4.2; fi
RUN if [ -f /tests/deps/requirements.txt ]; then python -m pip install -q -r /tests/deps/requirements.txt; fi
`
}

// untar extracts a HINT archive (regular files, directories and links whose targets stay
// inside) into dir. No write passes through a link, and every link must resolve inside.
func untar(data []byte, dir string) error {
	d, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return d.Check()
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		switch h.Typeflag {
		case tar.TypeReg:
			b, err := io.ReadAll(io.LimitReader(tr, 1<<30))
			if err != nil {
				return err
			}
			if err := d.WriteFile(name, b, os.FileMode(h.Mode&0o777)); err != nil {
				return fmt.Errorf("unsafe archive entry: %w", err)
			}
		case tar.TypeSymlink:
			if err := d.Symlink(name, h.Linkname); err != nil {
				return fmt.Errorf("unsafe link in archive: %w", err)
			}
		case tar.TypeDir:
			if err := d.Mkdir(name); err != nil {
				return fmt.Errorf("unsafe archive entry: %w", err)
			}
		default:
			return fmt.Errorf("unsupported archive entry type")
		}
	}
}

// DirDigest returns a digest of a task directory (paths, modes and contents), to compare
// an export with a later one.
func DirDigest(dir string) (string, error) {
	var lines []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			lines = append(lines, "L "+filepath.ToSlash(rel)+" "+t)
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("F %s %o %s", filepath.ToSlash(rel), fi.Mode().Perm(), sum(b)))
		}
		return nil
	})
	sort.Strings(lines)
	return "sha256:" + sum([]byte(strings.Join(lines, "\n"))), err
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}
