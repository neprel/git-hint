// Package readgit supplies callback-disabled, bounded Git safety inspections.
// It does not resolve complete source inventory or authorize a capture policy.
package readgit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const FallbackTimeout = 10 * time.Second
const MaxOutput int64 = 4 << 20
const MaxStderr int64 = 64 << 10

// Guard preserves an existing deadline. Legacy callers without one get one bounded guard;
// callers pass that same context through all nested checks, never renewed per command.
func Guard(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, FallbackTimeout)
}

// Args disables callback programs and paging independently of repository configuration.
func Args(root string, args ...string) []string {
	return append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-C", root}, args...)
}

// Env removes ambient command-config injections. Ordinary global/system config sources
// remain selected as before; the fixed argv overrides their callback settings.
func Env(base []string) []string {
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if key == "GIT_CONFIG_PARAMETERS" || key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") || key == "GIT_OPTIONAL_LOCKS" {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0")
}

func Command(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", Args(root, args...)...)
	cmd.Env = Env(os.Environ())
	cmd.WaitDelay = time.Second
	return cmd
}

type boundedOutput struct {
	b        bytes.Buffer
	cap      int64
	over     bool
	complete bool
	cancel   context.CancelFunc
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if int64(w.b.Len()+len(p)) > w.cap {
		w.over = true
		w.cancel()
		return 0, errors.New("git_output_limit")
	}
	return w.b.Write(p)
}

// ReadFrom records terminal EOF independently of Cmd.Wait: a nonzero process exit
// can otherwise mask WaitDelay's stream-copy error and expose an incomplete prefix.
func (w *boundedOutput) ReadFrom(r io.Reader) (int64, error) {
	var written int64
	buf := make([]byte, 32<<10)
	for {
		n, e := r.Read(buf)
		if n > 0 {
			done, we := w.Write(buf[:n])
			written += int64(done)
			if we != nil {
				return written, we
			}
		}
		if e == io.EOF {
			w.complete = true
			return written, nil
		}
		if e != nil {
			return written, e
		}
	}
}

// Output returns bytes only after a complete joined command with clean bounded stderr.
// Exit 1 is exposed for check-ignore's deliberate nonmatch semantics; every other error,
// timeout, bound or incomplete stream discards output rather than accepting a prefix.
func Output(parent context.Context, root string, limit int64, args ...string) ([]byte, error) {
	ctx, done := Guard(parent)
	defer done()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if limit < 1 {
		return nil, errors.New("git_output_limit")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &boundedOutput{cap: limit, cancel: cancel}
	stderr := &boundedOutput{cap: MaxStderr, cancel: cancel}
	cmd := Command(child, root, args...)
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if out.over || stderr.over {
		return nil, errors.New("git_output_limit")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !out.complete || !stderr.complete {
		return nil, errors.New("git_output_incomplete")
	}
	if stderr.b.Len() != 0 {
		return nil, errors.New("git_stderr")
	}
	if err != nil && !ExitOne(err) {
		return nil, err
	}
	return append([]byte(nil), out.b.Bytes()...), err
}
func ExitOne(err error) bool { var e *exec.ExitError; return errors.As(err, &e) && e.ExitCode() == 1 }
