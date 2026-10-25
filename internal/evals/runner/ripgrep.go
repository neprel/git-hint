package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// RipgrepSource is an explicit offline input for the pinned OpenCode study runtime.
// It has no download, PATH lookup, caller-selected pin or automatic fallback.
type RipgrepSource struct{ BinaryFile string }

const ripgrepVersion = "15.1.0"
const ripgrepTarget = "linux-arm64"
const ripgrepSize int64 = 4543848
const ripgrepSHA256 = "sha256:968cabe8efed72fd8fd482cb76b6084fcb695fc5293af7fb62296b02f487fb69"
const ripgrepArchiveBytes int64 = ripgrepSize + (64 << 10)
const ripgrepInstall = "opt/hint/bin/rg"

// The reviewed GNU aarch64 release requires the AArch64 GNU dynamic loader and
// GLIBC 2.17/2.18. Byte identity is not proof of availability in an actual image.
func ripgrepIdentity(target string) (any, error) {
	if target != ripgrepTarget {
		return nil, errors.New("offline ripgrep target is unsupported")
	}
	return []any{"opencode-offline-ripgrep-v1", ripgrepVersion, target, ripgrepSHA256, ripgrepSize, "/" + ripgrepInstall, "root:root:0755", "gnu-aarch64-glibc-2.17-2.18", "required_absent_before_install", "same_container_exact_file_after_install", ripgrepArchiveBytes, "strict_complete_tar_v1"}, nil
}

func (s RipgrepSource) binary(ctx context.Context, target string) ([]byte, error) {
	if _, err := ripgrepIdentity(target); err != nil {
		return nil, err
	}
	return readRipgrepBinary(ctx, s.BinaryFile, ripgrepSize, ripgrepSHA256)
}

// readRipgrepBinary is the one local file admission path. The private parameters
// permit small synthetic tests; production always supplies the compiled constants.
func readRipgrepBinary(ctx context.Context, name string, size int64, digest string) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, errors.New("offline ripgrep artifact is unverified") }
	if ctx.Err() != nil || name == "" || size <= 0 || size > ripgrepSize {
		return fail()
	}
	f, err := openRipgrepFile(name)
	if err != nil {
		return fail()
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != size {
		return fail()
	}
	named, err := os.Lstat(name)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(before, named) {
		return fail()
	}
	b := make([]byte, 0, int(size))
	buf := make([]byte, 64<<10)
	for {
		if ctx.Err() != nil {
			return fail()
		}
		left := size + 1 - int64(len(b))
		if left <= 0 {
			return fail()
		}
		chunk := buf
		if int64(len(chunk)) > left {
			chunk = chunk[:left]
		}
		n, e := f.Read(chunk)
		b = append(b, chunk[:n]...)
		if int64(len(b)) > size {
			return fail()
		}
		if e == io.EOF {
			break
		}
		if e != nil || n == 0 {
			return fail()
		}
	}
	after, err := f.Stat()
	if err != nil {
		return fail()
	}
	named, err = os.Lstat(name)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(before, named) || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) || named.Size() != before.Size() || named.Mode() != before.Mode() || !named.ModTime().Equal(before.ModTime()) {
		return fail()
	}
	err = f.Close()
	closed = true
	if err != nil || ctx.Err() != nil || int64(len(b)) != size {
		return fail()
	}
	h := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(h[:]) != digest {
		return fail()
	}
	return b, nil
}
