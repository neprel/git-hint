package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// B1 cli-file-v1 (07B-J §14/§15): a process-separated behavior check. A declared command
// runs once per case with exact stdin bytes; its stdout bytes and exit status are compared
// with the reference's on the same input. Every run is a fresh container (network none,
// limits) that holds only one frozen tree and gets the input on stdin; the runner captures
// stdout and the exit status through the engine API and compares on the host. No
// candidate or reference code runs where the verdict is made. The verdict is equivalence
// to the stored reference on the declared finite cases, not universal correctness.

// IOInterface is the only B1 interface.
const IOInterface = "cli-file-v1"

// Limits of a B1 task.
const (
	maxIOCases       = 64
	maxIOBytes       = 1 << 20
	maxIOCaseSeconds = 30
	maxEntryArgs     = 16
	// OracleRuns is the number of reference runs per case in every trial (fixed before the
	// start); differing reference runs make the trial unknown.
	OracleRuns = 2
)

var ioCaseID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ioSpec is tests/io/spec.json, written by the task author.
type ioSpec struct {
	Interface   string   `json:"interface"`
	Entry       []string `json:"entry"`
	Cases       []string `json:"cases"`
	CaseSeconds int      `json:"case_seconds"`
	Compare     string   `json:"compare"`
}

// ioTask is a checked B1 declaration with its inputs and their digests.
type ioTask struct {
	spec         ioSpec
	inputs       map[string][]byte
	specDigest   string
	inputsDigest string
}

// readIOTask reads the B1 declaration of a task, or nil when the task has none. A
// declaration that is not exactly valid refuses the task: unknown fields, an interface or
// comparison other than the one, 1..64 unique safe case IDs, a bounded entry, every input
// a regular file of at most 1 MiB, and no other file next to the inputs.
func readIOTask(taskDir string) (*ioTask, error) {
	dir := filepath.Join(taskDir, "tests", "io")
	b, err := os.ReadFile(filepath.Join(dir, "spec.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bad := func(why string) (*ioTask, error) { return nil, fmt.Errorf("the B1 declaration is invalid: %s", why) }
	if len(b) > 64<<10 {
		return bad("spec too large")
	}
	var s ioSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return bad("spec is not one strict JSON object")
	}
	if s.Interface != IOInterface || s.Compare != "exact" {
		return bad("interface or comparison")
	}
	if len(s.Entry) == 0 || len(s.Entry) > maxEntryArgs {
		return bad("entry")
	}
	for _, a := range s.Entry {
		if a == "" || len(a) > 256 || strings.ContainsRune(a, 0) {
			return bad("entry argument")
		}
	}
	if s.CaseSeconds < 1 || s.CaseSeconds > maxIOCaseSeconds {
		return bad("case_seconds")
	}
	if len(s.Cases) == 0 || len(s.Cases) > maxIOCases {
		return bad("number of cases")
	}
	t := &ioTask{spec: s, inputs: map[string][]byte{}}
	h := sha256.New()
	for _, id := range s.Cases {
		if !ioCaseID.MatchString(id) {
			return bad("case ID")
		}
		if _, dup := t.inputs[id]; dup {
			return bad("duplicate case ID")
		}
		p := filepath.Join(dir, "inputs", id+".in")
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxIOBytes {
			return bad("input of a case")
		}
		in, err := os.ReadFile(p)
		if err != nil || int64(len(in)) != fi.Size() {
			return bad("input of a case")
		}
		t.inputs[id] = in
		fmt.Fprintf(h, "%s\x00%d\x00", id, len(in))
		h.Write(in)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "inputs"))
	if err != nil || len(ents) != len(s.Cases) {
		return bad("unexpected files next to the inputs")
	}
	sum := sha256.Sum256(b)
	t.specDigest = "sha256:" + hex.EncodeToString(sum[:])
	t.inputsDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return t, nil
}

// caseRun is what the runner captured of one run.
type caseRun struct {
	out       []byte
	status    int64
	timedOut  bool
	truncated bool
}

func (c caseRun) usable() bool { return !c.timedOut && !c.truncated }

// errIOVerdict marks a B1 trial without a verdict: the reference could not be established
// (an oracle failure is never a candidate result).
type errIOVerdict struct{ why string }

func (e errIOVerdict) Error() string { return "no B1 verdict: " + e.why }

// ioPhase runs the B1 check of one trial within the one shared verifier deadline: the
// reference tree is materialized from the stored reference and frozen; each case runs
// OracleRuns times on it and once on the candidate's frozen tree, each run in a fresh
// container; the runner compares bytes and exit status. It writes the CHECK/TEST lines to
// verifier/test-stdout.txt and returns the reward and the provenance.
func (d *Docker) ioPhase(ctx context.Context, taskDir string, t *ioTask, lim taskLimits, img, candidate, dir string) (float64, map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, lim.verifier)
	defer cancel()
	prov := map[string]any{"interface": IOInterface, "spec": t.specDigest, "inputs": t.inputsDigest, "oracle_runs": OracleRuns,
		"verdict_scope": "equivalence_to_reference_on_declared_cases"}
	deadline := func() error {
		if ctx.Err() != nil {
			return timeoutError{"verifier"}
		}
		return nil
	}
	// the reference: the stored solution applied to the base in its own container, frozen
	reference := filepath.Join(dir, "oracle", "final.tar")
	rd, err := d.agentPhase(ctx, taskDir, "oracle", lim, img, reference)
	if err != nil {
		if deadline() != nil {
			return 0, prov, timeoutError{"verifier"}
		}
		return 0, prov, errIOVerdict{"reference tree: " + err.Error()}
	}
	prov["reference_tree"] = rd
	limit := time.Duration(t.spec.CaseSeconds) * time.Second
	expected := map[string]caseRun{}
	oh := sha256.New()
	for _, id := range t.spec.Cases {
		var first caseRun
		for k := 0; k < OracleRuns; k++ {
			r, err := d.runCase(ctx, img, reference, t.spec.Entry, t.inputs[id], limit, lim)
			if err := deadline(); err != nil {
				return 0, prov, err
			}
			if err != nil || !r.usable() {
				return 0, prov, errIOVerdict{fmt.Sprintf("reference run of case %s failed", id)}
			}
			if k > 0 && (r.status != first.status || !bytes.Equal(r.out, first.out)) {
				return 0, prov, errIOVerdict{fmt.Sprintf("reference runs of case %s differ", id)}
			}
			first = r
		}
		expected[id] = first
		fmt.Fprintf(oh, "%s\x00%d\x00%d\x00", id, first.status, len(first.out))
		oh.Write(first.out)
	}
	prov["oracle_outputs"] = "sha256:" + hex.EncodeToString(oh.Sum(nil))
	var lines []string
	pass := true
	ch := sha256.New() // the captured candidate results, sealed on the host
	for _, id := range t.spec.Cases {
		r, err := d.runCase(ctx, img, candidate, t.spec.Entry, t.inputs[id], limit, lim)
		if err := deadline(); err != nil {
			return 0, prov, err
		}
		if err != nil {
			return 0, prov, err // the engine failed: no verdict
		}
		e := expected[id]
		st := "PASS"
		if !r.usable() || r.status != e.status || !bytes.Equal(r.out, e.out) {
			st, pass = "FAIL", false
		}
		lines = append(lines, "TEST B1 cases/"+id+"::output "+st)
		fmt.Fprintf(ch, "%s\x00%d\x00%t\x00%t\x00%d\x00", id, r.status, r.timedOut, r.truncated, len(r.out))
		ch.Write(r.out)
	}
	prov["candidate_outputs"] = "sha256:" + hex.EncodeToString(ch.Sum(nil))
	check := "FAIL"
	reward := 0.0
	if pass {
		check, reward = "PASS", 1
	}
	lines = append(lines, "CHECK B1 "+check)
	if err := os.WriteFile(filepath.Join(dir, "verifier", "test-stdout.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return 0, prov, err
	}
	return reward, prov, nil
}

// caseScript runs the entry on exactly the frozen tree: the tree arrives under /hint/app
// and replaces the image's /app before the entry starts.
const caseScript = `rm -rf /app && mv /hint/app /app && cd /app && exec "$@"`

// runCase runs the entry once in a fresh container on one frozen tree with the input on
// stdin, and captures stdout (at most 1 MiB; more is truncation) and the exit status
// through the engine API. stderr is drained and dropped. An engine failure is an error;
// the run's own timeout or truncation is recorded in the result.
func (d *Docker) runCase(ctx context.Context, img, tree string, entry []string, input []byte, limit time.Duration, lim taskLimits) (caseRun, error) {
	cmd := append([]string{"sh", "-c", caseScript, "sh"}, entry...)
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: img, Cmd: cmd, WorkingDir: "/", OpenStdin: true, StdinOnce: true,
			AttachStdin: true, AttachStdout: true, AttachStderr: true},
		HostConfig: d.hostConfig(lim)})
	if err != nil {
		return caseRun{}, err
	}
	defer d.remove(c.ID)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(retree(tree, "hint/", pw)) }()
	_, err = d.cli.CopyToContainer(ctx, c.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: pr})
	pr.Close()
	if err != nil {
		return caseRun{}, err
	}
	att, err := d.cli.ContainerAttach(ctx, c.ID, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return caseRun{}, err
	}
	defer att.Close()
	out := &capWriter{max: maxIOBytes}
	done := make(chan struct{})
	go func() {
		_, _ = stdcopy.StdCopy(out, io.Discard, att.Reader)
		close(done)
	}()
	go func() {
		_, _ = att.Conn.Write(input)
		_ = att.CloseWrite()
	}()
	var r caseRun
	err = d.runToEnd(ctx, c.ID, limit, "case")
	var ee exitError
	var te timeoutError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.status = ee.code
	case errors.As(err, &te):
		r.timedOut = true
	default:
		return caseRun{}, err
	}
	select { // the stream ends when the container stopped; a stream that does not end is not trusted
	case <-done:
	case <-time.After(5 * time.Second):
		r.truncated = true
	}
	r.out, r.truncated = out.buf.Bytes(), r.truncated || out.over
	return r, nil
}

// capWriter keeps at most max bytes and records any excess.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.over = true
	} else {
		c.buf.Write(p)
	}
	return len(p), nil
}

// retree copies a frozen tree archive (entries under app/) with a prefix added to every
// name and hard-link target; the archive was checked when it was frozen (C3).
func retree(artifact, prefix string, w io.Writer) error {
	f, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	tw := tar.NewWriter(w)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		h.Name = prefix + h.Name
		if h.Typeflag == tar.TypeLink {
			h.Linkname = prefix + h.Linkname
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}
