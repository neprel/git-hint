package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/neprel/git-hint/internal/evals/iodecl"
)

// B1 cli-file-v1 (07B-J §14/§15): a process-separated behavior check. A declared command
// runs once per case with exact stdin bytes; its stdout bytes and exit status are compared
// with the reference's on the same input. Every run is a fresh container (network none,
// limits) that holds only one frozen tree and gets the input on stdin; the runner captures
// stdout and the exit status through the engine API and compares on the host. No
// candidate or reference code runs where the verdict is made. The verdict is equivalence
// to the stored reference on the declared finite cases, not universal correctness.

// IOInterface is the only B1 interface.
const IOInterface = iodecl.Interface

// Limits of a B1 task.
const (
	maxIOCases = iodecl.MaxCases
	maxIOBytes = iodecl.MaxBytes
	// OracleRuns is the number of reference runs per case in every trial (fixed before the
	// start); differing reference runs make the trial unknown.
	OracleRuns = 2
)

// ioTask is a checked B1 declaration.
type ioTask struct {
	spec         iodecl.Spec
	inputs       map[string][]byte
	specDigest   string
	inputsDigest string
}

// readIOTask reads the B1 declaration of a task, or nil when the task has none. A
// declaration that is not exactly valid refuses the task (iodecl.Parse); every input must be a
// regular file <id>.in of at most 1 MiB with no other file next to the inputs, and the
// stored reference must be on the verifier side (tests/io/reference).
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
	ents, err := os.ReadDir(filepath.Join(dir, "inputs"))
	if err != nil || len(ents) > maxIOCases {
		return bad("inputs")
	}
	inputs := map[string][]byte{}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".in")
		p := filepath.Join(dir, "inputs", e.Name())
		fi, err := os.Lstat(p)
		if !ok || err != nil || !fi.Mode().IsRegular() || fi.Size() > maxIOBytes {
			return bad("input file")
		}
		in, err := os.ReadFile(p)
		if err != nil || int64(len(in)) != fi.Size() {
			return bad("input file")
		}
		inputs[id] = in
	}
	if fi, err := os.Lstat(filepath.Join(dir, "reference", "solve.sh")); err != nil || !fi.Mode().IsRegular() {
		return bad("no stored reference (tests/io/reference/solve.sh)")
	}
	return parseIO(b, inputs)
}

// parseIO checks a declaration with the shared rules (iodecl.Parse).
func parseIO(spec []byte, inputs map[string][]byte) (*ioTask, error) {
	d, err := iodecl.Parse(spec, inputs)
	if err != nil {
		return nil, err
	}
	return &ioTask{spec: d.Spec, inputs: d.Inputs, specDigest: d.SpecDigest, inputsDigest: d.InputsDigest}, nil
}

// caseRun is what the runner captured of one run.
type caseRun struct {
	out         []byte
	status      int64
	timedOut    bool
	truncated   bool
	inputFailed bool // the input was not delivered completely
	networkNone bool // the engine reports network mode none and no attached network
}

func (c caseRun) usable() bool { return !c.timedOut && !c.truncated && !c.inputFailed }

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
	// the stored reference lives on the verifier side (tests/io/reference): qualification
	// variants that replace solution/ (the candidate) never change it
	reference := filepath.Join(dir, "oracle", "final.tar")
	rd, err := d.applyPhase(ctx, filepath.Join(taskDir, "tests", "io", "reference"), "oracle", lim, img, reference)
	if err != nil {
		if deadline() != nil {
			return 0, prov, timeoutError{"verifier"}
		}
		return 0, prov, errIOVerdict{"reference tree: " + err.Error()}
	}
	prov["reference_tree"] = rd
	limit := time.Duration(t.spec.CaseSeconds) * time.Second
	return compareFrozenIO(ctx, t, dir, prov, false, func(oracle bool, id string, repetition int) (caseRun, error) {
		tree := candidate
		if oracle {
			tree = reference
		}
		return d.runCase(ctx, img, tree, t.spec.Entry, t.inputs[id], limit, lim)
	})
}

// compareFrozenIO is the sole B1 reference/candidate comparison and output recipe.
// Its executor is private runner plumbing, never a public caller-supplied source.
func compareFrozenIO(ctx context.Context, t *ioTask, dir string, prov map[string]any, strict bool,
	run func(oracle bool, id string, repetition int) (caseRun, error)) (float64, map[string]any, error) {
	deadline := func() error {
		if ctx.Err() != nil {
			return timeoutError{"verifier"}
		}
		return nil
	}
	allNone := true // every run's networks as the engine reports them
	expected, outputs, allNone, err := referenceExpectations(t, func(id string, repetition int) (caseRun, error) {
		r, e := run(true, id, repetition)
		if de := deadline(); de != nil {
			return r, referenceOverallDeadline{de}
		}
		if strict && !r.networkNone {
			return r, errIOVerdict{"reference network not blocked"}
		}
		return r, e
	})
	if err != nil {
		return 0, prov, err
	}
	prov["oracle_outputs"] = outputs
	var lines []string
	pass := true
	ch := sha256.New() // the captured candidate results, sealed on the host
	for _, id := range t.spec.Cases {
		r, err := run(false, id, 0)
		if err := deadline(); err != nil {
			return 0, prov, err
		}
		if err != nil {
			return 0, prov, err // the engine failed: no verdict
		}
		if strict && (!r.usable() || !r.networkNone) {
			return 0, prov, errIOVerdict{"candidate execution incomplete or network not blocked"}
		}
		allNone = allNone && r.networkNone
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
	// the network record of the B1 runs: from the engine's inspection of every run (no
	// in-container measurement; nothing in the runs can write it)
	net := "reachable"
	if allNone {
		net = "blocked"
	}
	prov["network"] = "engine_inspect:" + net
	if err := os.WriteFile(filepath.Join(dir, "verifier", "network.txt"), []byte("HINT_VERIFIER_NET="+net+"\n"), 0o600); err != nil {
		return 0, prov, err
	}
	return reward, prov, nil
}

// caseScript runs the entry on exactly the frozen tree, unprivileged (C7BJ-7): as root it
// only replaces the image's /app with the tree that arrived under /hint/app and hands it to
// the unprivileged user; then the entry is executed as uid/gid 65534 with no supplementary
// groups and no capabilities (inheritable and bounding sets cleared; no-new-privileges is
// set on the container). An image without setpriv cannot run a B1 case.
const caseScript = `rm -rf /app && mv /hint/app /app && chown -R 65534:65534 /app && cd /app && ` +
	`exec setpriv --reuid=65534 --regid=65534 --clear-groups --inh-caps=-all --bounding-set=-all -- env HOME=/tmp "$@"`

// runCase runs the entry once in a fresh container on one frozen tree with the input on
// stdin, and captures stdout (at most 1 MiB; more is truncation) and the exit status
// through the engine API. stderr is drained and dropped. The I/O goroutines are always
// stopped and joined before the result is read (C7BJ-5): an engine failure or a broken
// output stream is an error (no result); an input that was not fully delivered, the run's
// timeout or truncation is recorded in the result and makes it unusable.
func (d *Docker) runCase(ctx context.Context, img, tree string, entry []string, input []byte, limit time.Duration, lim taskLimits) (caseRun, error) {
	cmd := append([]string{"sh", "-c", caseScript, "sh"}, entry...)
	cid, err := d.create(ctx, &container.Config{Image: img, Cmd: cmd, WorkingDir: "/", OpenStdin: true, StdinOnce: true,
		AttachStdin: true, AttachStdout: true, AttachStderr: true}, d.hostConfig(lim))
	if err != nil {
		return caseRun{}, err
	}
	defer d.removeForPhase(ctx, cid)
	phase := referencePhaseFrom(ctx)
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		var e error
		if phase == nil {
			e = retree(tree, "hint/", pw)
		} else {
			e = referenceRetree(ctx, tree, phase.artifactDigest, "hint/", pw, phase.archiveCap)
		}
		pw.CloseWithError(e)
		copied <- e
	}()
	_, err = d.cli.CopyToContainer(ctx, cid, client.CopyToContainerOptions{DestinationPath: "/", Content: pr})
	pr.Close()
	if phase != nil {
		phase.join(copied)
		if phase.err != nil {
			return caseRun{}, phase.err
		}
	}
	if err != nil {
		return caseRun{}, err
	}
	att, err := d.cli.ContainerAttach(ctx, cid, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return caseRun{}, err
	}
	out := &capWriter{max: maxIOBytes}
	readErr := make(chan error, 1)
	go func() {
		readErr <- demux(out, io.Discard, att.Reader)
	}()
	wrote := make(chan error, 1)
	go func() {
		_, err := att.Conn.Write(input)
		if cerr := att.CloseWrite(); err == nil {
			err = cerr
		}
		wrote <- err
	}()
	var r caseRun
	if phase != nil {
		_ = att.Conn.SetDeadline(phase.joinEnd)
	}
	runErr := d.runToEnd(ctx, cid, limit, "case")
	// the engine's own record of the run's networks: only "none" with no attached network
	if ins, err := d.cli.ContainerInspect(inspectionContext(ctx), cid, client.ContainerInspectOptions{}); err == nil && ins.Container.HostConfig != nil {
		r.networkNone = ins.Container.HostConfig.NetworkMode == "none" &&
			(ins.Container.NetworkSettings == nil || len(ins.Container.NetworkSettings.Networks) == 0 || onlyNone(ins.Container.NetworkSettings.Networks))
	}
	var ee exitError
	var te timeoutError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		r.status = ee.code
	case errors.As(runErr, &te):
		r.timedOut = true
	}
	// the container has stopped (or failed): the stream must end on its own; if it does not
	// within the bound, the connection is closed; both goroutines are joined either way
	var rerr error
	if phase == nil {
		select {
		case rerr = <-readErr:
		case <-time.After(5 * time.Second):
			att.Close()
			<-readErr
			rerr = errors.New("the output stream did not end")
		}
	} else {
		select {
		case rerr = <-readErr:
		case <-ctx.Done():
			att.Close()
			phase.join(readErr)
			rerr = ctx.Err()
		case <-streamEnd(ctx, 0):
			att.Close()
			phase.join(readErr)
			rerr = errReferencePrepare
		}
	}
	att.Close()
	var werr error
	if phase == nil {
		werr = <-wrote
	} else {
		werr = phase.join(wrote)
		if phase.err != nil {
			return caseRun{}, phase.err
		}
	}
	if runErr != nil && !r.timedOut && !errors.As(runErr, &ee) {
		return caseRun{}, runErr
	}
	if rerr != nil {
		return caseRun{}, fmt.Errorf("incomplete output stream: %w", rerr)
	}
	r.inputFailed = werr != nil
	r.out, r.truncated = bytes.Clone(out.buf.Bytes()), out.over
	return r, nil
}

// demux reads the engine's multiplexed attach stream strictly: stdout frames go to stdout,
// stderr frames to stderr; the stream may end only at a frame boundary. A cut header or
// body, an unknown stream type or a system-error frame is an error (stdcopy treats an EOF
// inside a frame as the end).
func demux(stdout, stderr io.Writer, r io.Reader) error {
	var hdr [8]byte
	for {
		n, err := io.ReadFull(r, hdr[:])
		if n == 0 && err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cut frame header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		var w io.Writer
		switch hdr[0] {
		case 1:
			w = stdout
		case 2:
			w = stderr
		default:
			return fmt.Errorf("unexpected stream type %d", hdr[0])
		}
		if m, err := io.CopyN(w, r, size); err != nil || m != size {
			return fmt.Errorf("cut frame body: %v", err)
		}
	}
}

// onlyNone says whether the attached networks are just the engine's "none" network.
func onlyNone(m map[string]*network.EndpointSettings) bool {
	for k := range m {
		if k != "none" {
			return false
		}
	}
	return true
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
	return retreeFrom(f, prefix, w)
}

func retreeFrom(r io.Reader, prefix string, w io.Writer) error {
	tr := tar.NewReader(r)
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

// referenceExpectations is the one B1 oracle agreement and output-digest recipe.
// Preparation and ordinary ioPhase use the same case order and OracleRuns.
// IOTranscriptCase contains one already-established output in declared order.
// IOTranscriptDigest is framing only; it does not run or verify a program.
type IOTranscriptCase struct {
	ID     string
	Status int64
	Output []byte
}

func IOTranscriptDigest(cases []IOTranscriptCase) (string, error) {
	if len(cases) < 1 || len(cases) > maxIOCases {
		return "", errors.New("io_transcript_count")
	}
	h := sha256.New()
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] || len(c.Output) > maxIOBytes {
			return "", errors.New("io_transcript_case")
		}
		seen[c.ID] = true
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", c.ID, c.Status, len(c.Output))
		h.Write(c.Output)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func referenceExpectations(t *ioTask, run func(string, int) (caseRun, error)) (map[string]caseRun, string, bool, error) {
	expected := map[string]caseRun{}
	transcript := []IOTranscriptCase{}
	allNone := true
	for _, id := range t.spec.Cases {
		var first caseRun
		for k := 0; k < OracleRuns; k++ {
			r, err := run(id, k)
			if err != nil || !r.usable() || r.status != 0 {
				var overall referenceOverallDeadline
				if errors.As(err, &overall) {
					return nil, "", false, overall.err
				}
				return nil, "", false, errIOVerdict{fmt.Sprintf("reference run of case %s failed", id)}
			}
			allNone = allNone && r.networkNone
			if k > 0 && (r.status != first.status || !bytes.Equal(r.out, first.out)) {
				return nil, "", false, errIOVerdict{fmt.Sprintf("reference runs of case %s differ", id)}
			}
			first = r
		}
		expected[id] = first
		transcript = append(transcript, IOTranscriptCase{id, first.status, first.out})
	}
	digest, e := IOTranscriptDigest(transcript)
	return expected, digest, allNone, e
}

// Only ioPhase's own overall deadline has legacy timeout classification. A per-case
// runner timeout remains an oracle-verdict failure, as before the loop extraction.
type referenceOverallDeadline struct{ err error }

func (e referenceOverallDeadline) Error() string { return e.err.Error() }
func (e referenceOverallDeadline) Unwrap() error { return e.err }
