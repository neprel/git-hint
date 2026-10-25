package runner

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

var prelaunchDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const prelaunchCaptureBound = time.Minute

// Raw source/archive bound includes headers, extension metadata, payload padding, EOS
// and zero record padding. It is separate from the 2 GiB file-content/200,000-entry bounds.
const maxPrelaunchArchiveBytes int64 = maxTreeBytes + maxTreeFiles*2048 + 1024

// PrelaunchTreeCapture is local audit metadata, not public producer evidence. Its offsets
// use the same Counter anchor as agent_run. No container identifier or source bytes enter it.
type PrelaunchTreeCapture struct {
	State           string `json:"state"`
	CheckState      string `json:"check_state,omitempty"`
	CheckStartedMs  int64  `json:"check_started_ms,omitempty"`
	CheckFinishedMs int64  `json:"check_finished_ms,omitempty"`
	Digest          string `json:"digest,omitempty"`
	Clock           string `json:"clock"`
	StartedMs       int64  `json:"started_ms"`
	FinishedMs      int64  `json:"finished_ms"`
	ExecutionDigest string `json:"execution_digest"`
}

func (d *Docker) capturePrelaunchTree(ctx context.Context, id, artifact, execution string, c *Counter) (*PrelaunchTreeCapture, error) {
	rec := &PrelaunchTreeCapture{State: "failed", Clock: "ledger_anchor_monotonic_ms", ExecutionDigest: execution, StartedMs: c.mark().Milliseconds()}
	defer func() { rec.FinishedMs = c.mark().Milliseconds() }()
	fail := errors.New("prelaunch tree capture failed")
	if !prelaunchDigestPattern.MatchString(execution) || os.MkdirAll(filepath.Dir(artifact), 0o700) != nil {
		return rec, fail
	}
	bounded, cancel := context.WithTimeout(ctx, prelaunchCaptureBound)
	defer cancel()
	src, err := d.cli.CopyFromContainer(bounded, id, client.CopyFromContainerOptions{SourcePath: "/app"})
	if err != nil {
		return rec, fail
	}
	digest, err := freezeTree(src.Content, artifact)
	closeErr := src.Content.Close()
	if err != nil {
		return rec, fail
	}
	if closeErr != nil || bounded.Err() != nil {
		_ = os.Remove(artifact)
		return rec, fail
	}
	rec.State, rec.Digest = "complete", digest
	return rec, nil
}

// MaterializePrelaunchTree checks exact local artifact bytes and the existing freezeTree
// bounds before populating an empty destination. It materializes only regular files and
// directories; link aliases remain unresolved. It never follows an archive link or executes
// source. The caller owns destination and its parent, excludes concurrent writers, and
// removes the temporary destination on failure. No foreign entries are deleted.
func MaterializePrelaunchTree(artifact, expected, destination string) (files []string, err error) {
	bad := errors.New("invalid observed-start artifact")
	if !prelaunchDigestPattern.MatchString(expected) {
		return nil, bad
	}
	stat, e := os.Lstat(destination)
	if e != nil || !stat.IsDir() {
		return nil, bad
	}
	entries, e := os.ReadDir(destination)
	if e != nil || len(entries) != 0 {
		return nil, bad
	}
	input, e := openPrelaunchInput(artifact)
	if e != nil {
		return nil, bad
	}
	defer input.Close()
	inv, e := materializePrelaunch(context.Background(), input, expected, destination)
	if e != nil {
		return nil, e
	}
	files = inv.Files()
	if len(files) == 0 {
		return nil, nil
	}
	return files, nil
}

func openPrelaunchInput(artifact string) (*os.File, error) {
	bad := errors.New("invalid observed-start artifact")
	stat, e := os.Lstat(artifact)
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > maxPrelaunchArchiveBytes {
		return nil, bad
	}
	input, e := os.Open(artifact)
	if e != nil {
		return nil, bad
	}
	opened, e := input.Stat()
	if e != nil || !opened.Mode().IsRegular() || !os.SameFile(stat, opened) {
		input.Close()
		return nil, bad
	}
	return input, nil
}

func materializePrelaunch(ctx context.Context, input io.Reader, expected, destination string) (inventory PrelaunchInventory, err error) {
	bad := errors.New("invalid observed-start artifact")
	if ctx.Err() != nil || !prelaunchDigestPattern.MatchString(expected) {
		return inventory, bad
	}
	st, e := os.Lstat(destination)
	if e != nil || !st.IsDir() {
		return inventory, bad
	}
	entries, e := os.ReadDir(destination)
	if e != nil || len(entries) != 0 {
		return inventory, bad
	}
	private, e := os.MkdirTemp(filepath.Dir(destination), ".prelaunch-check-")
	if e != nil {
		return PrelaunchInventory{}, e
	}
	defer os.RemoveAll(private)
	snapshot, e := os.OpenFile(filepath.Join(private, "input.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if e != nil {
		return PrelaunchInventory{}, e
	}
	defer snapshot.Close()
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(snapshot, hash), io.LimitReader(prelaunchContextReader{ctx, input}, maxPrelaunchArchiveBytes+1))
	if e != nil || n > maxPrelaunchArchiveBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected {
		return PrelaunchInventory{}, bad
	}
	if _, e = snapshot.Seek(0, io.SeekStart); e != nil {
		return PrelaunchInventory{}, e
	}
	checked := filepath.Join(private, "checked.tar")
	digest, e := freezeTree(prelaunchContextReader{ctx, snapshot}, checked)
	// Capture artifacts originate from freezeTree; reject trailing bytes and noncanonical
	// input as well as malformed trees. A changed artifact cannot acquire a new identity here.
	if e != nil || digest != expected {
		return PrelaunchInventory{}, bad
	}
	archive, e := os.Open(checked)
	if e != nil {
		return PrelaunchInventory{}, e
	}
	defer archive.Close()

	inventory.Digest = expected
	tr := tar.NewReader(prelaunchContextReader{ctx, archive})
	for {
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return PrelaunchInventory{}, bad
		}
		name, ok := treePath(header.Name)
		if !ok {
			return PrelaunchInventory{}, bad
		}
		entry := PrelaunchEntry{Path: name, Type: header.Typeflag, Mode: header.Mode, Size: header.Size, UID: header.Uid, GID: header.Gid, LinkTarget: header.Linkname}
		if header.Typeflag == tar.TypeReg {
			entry.LogicalMode = 0o100644
			if header.Mode&0o111 != 0 {
				entry.LogicalMode = 0o100755
			}
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(target, 0o700); e != nil {
				return PrelaunchInventory{}, e
			}
		case tar.TypeReg:
			if e = os.MkdirAll(filepath.Dir(target), 0o700); e != nil {
				return PrelaunchInventory{}, e
			}
			f, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600|os.FileMode(header.Mode)&0o111)
			if e != nil {
				return PrelaunchInventory{}, e
			}
			h := sha256.New()
			_, e = io.Copy(io.MultiWriter(f, h), tr)
			entry.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
			closeErr := f.Close()
			if e != nil || closeErr != nil {
				return PrelaunchInventory{}, bad
			}
		default:
			entry.Unresolved = true
		}
		inventory.Entries = append(inventory.Entries, entry)
	}
	if ctx.Err() != nil {
		return PrelaunchInventory{}, ctx.Err()
	}
	sort.Slice(inventory.Entries, func(i, j int) bool { return inventory.Entries[i].Path < inventory.Entries[j].Path })
	return inventory, nil
}

// PrelaunchEntry describes the captured archive, never scratch permissions. Aliases
// remain unresolved and carry no inferred regular-file identity.
type PrelaunchEntry struct {
	UID, GID                int
	LinkTarget              string
	Path                    string
	Type                    byte
	Mode, LogicalMode, Size int64
	Digest                  string
	Unresolved              bool
}
type PrelaunchInventory struct {
	Digest  string
	Entries []PrelaunchEntry
}

func (i PrelaunchInventory) Copy() PrelaunchInventory {
	if i.Entries != nil {
		entries := make([]PrelaunchEntry, len(i.Entries))
		copy(entries, i.Entries)
		i.Entries = entries
	}
	return i
}
func (i PrelaunchInventory) Files() []string {
	out := []string{}
	for _, e := range i.Entries {
		if e.Type == tar.TypeReg {
			out = append(out, e.Path)
		}
	}
	return out
}

// PrelaunchArtifact is an ephemeral capability over retained actual capture bytes.
// Only the runner constructs it. Retaining this pointer does not extend its lifetime.
type PrelaunchArtifact struct {
	ctx     context.Context
	mu      sync.Mutex
	file    *os.File
	dir     string
	capture PrelaunchTreeCapture
	bytes   int64
}
type PrelaunchArtifactMetadata struct {
	Capture PrelaunchTreeCapture
	Bytes   int64
}

func (a *PrelaunchArtifact) Metadata() PrelaunchArtifactMetadata {
	if a == nil {
		return PrelaunchArtifactMetadata{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return PrelaunchArtifactMetadata{a.capture, a.bytes}
}
func (*PrelaunchArtifact) MarshalJSON() ([]byte, error) {
	return nil, errors.New("prelaunch artifact is local only")
}
func (a *PrelaunchArtifact) Materialize(ctx context.Context, destination string) (PrelaunchInventory, error) {
	if a == nil {
		return PrelaunchInventory{}, errors.New("prelaunch artifact unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil || ctx.Err() != nil {
		return PrelaunchInventory{}, errors.New("prelaunch artifact unavailable")
	}
	bounded, cancel := context.WithCancel(a.ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if ctx.Err() != nil {
		cancel()
	}
	return materializePrelaunch(bounded, io.NewSectionReader(a.file, 0, a.bytes), a.capture.Digest, destination)
}
func (a *PrelaunchArtifact) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return errors.New("prelaunch artifact already closed")
	}
	e := a.file.Close()
	a.file = nil
	r := os.RemoveAll(a.dir)
	if e != nil {
		return e
	}
	return r
}
func retainPrelaunch(ctx context.Context, path string, capture PrelaunchTreeCapture) (*PrelaunchArtifact, error) {
	bad := errors.New("prelaunch retention failed")
	if capture.State != "complete" || !prelaunchDigestPattern.MatchString(capture.Digest) {
		return nil, bad
	}
	input, e := openRetainedPrelaunchFile(path)
	if e != nil {
		return nil, bad
	}
	defer input.Close()
	opened, e := input.Stat()
	if e != nil || !opened.Mode().IsRegular() || opened.Size() > maxPrelaunchArchiveBytes {
		return nil, bad
	}
	dir, e := os.MkdirTemp(filepath.Dir(path), ".prelaunch-retained-")
	if e != nil {
		return nil, bad
	}
	f, e := os.OpenFile(filepath.Join(dir, "capture"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if e != nil {
		os.RemoveAll(dir)
		return nil, bad
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(prelaunchContextReader{ctx, input}, maxPrelaunchArchiveBytes+1))
	if e != nil || ctx.Err() != nil || n > maxPrelaunchArchiveBytes || "sha256:"+hex.EncodeToString(h.Sum(nil)) != capture.Digest {
		f.Close()
		os.RemoveAll(dir)
		return nil, bad
	}
	return &PrelaunchArtifact{ctx: ctx, file: f, dir: dir, capture: capture, bytes: n}, nil
}

type prelaunchContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r prelaunchContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

func consumePrelaunch(ctx context.Context, a *PrelaunchArtifact, check func(context.Context, *PrelaunchArtifact) error) (err error) {
	defer func() {
		if e := a.close(); e != nil && err == nil {
			err = e
		}
	}()
	return check(ctx, a)
}
