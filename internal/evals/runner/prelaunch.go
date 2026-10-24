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
	stat, e = os.Lstat(artifact)
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > maxPrelaunchArchiveBytes {
		return nil, bad
	}
	input, e := os.Open(artifact)
	if e != nil {
		return nil, bad
	}
	defer input.Close()
	opened, e := input.Stat()
	if e != nil || !opened.Mode().IsRegular() || !os.SameFile(stat, opened) {
		return nil, bad
	}
	private, e := os.MkdirTemp(filepath.Dir(destination), ".prelaunch-check-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(private)
	snapshot, e := os.OpenFile(filepath.Join(private, "input.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if e != nil {
		return nil, e
	}
	defer snapshot.Close()
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(snapshot, hash), io.LimitReader(input, maxPrelaunchArchiveBytes+1))
	if e != nil || n > maxPrelaunchArchiveBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != expected {
		return nil, bad
	}
	if _, e = snapshot.Seek(0, io.SeekStart); e != nil {
		return nil, e
	}
	checked := filepath.Join(private, "checked.tar")
	digest, e := freezeTree(snapshot, checked)
	// Capture artifacts originate from freezeTree; reject trailing bytes and noncanonical
	// input as well as malformed trees. A changed artifact cannot acquire a new identity here.
	if e != nil || digest != expected {
		return nil, bad
	}
	archive, e := os.Open(checked)
	if e != nil {
		return nil, e
	}
	defer archive.Close()

	tr := tar.NewReader(archive)
	for {
		header, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, bad
		}
		name, ok := treePath(header.Name)
		if !ok {
			return nil, bad
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(target, 0o700); e != nil {
				return nil, e
			}
		case tar.TypeReg:
			if e = os.MkdirAll(filepath.Dir(target), 0o700); e != nil {
				return nil, e
			}
			f, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600|os.FileMode(header.Mode)&0o111)
			if e != nil {
				return nil, e
			}
			_, e = io.Copy(f, tr)
			closeErr := f.Close()
			if e != nil || closeErr != nil {
				return nil, bad
			}
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files, nil
}
