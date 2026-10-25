package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/neprel/git-hint/internal/evals/pipeline"
	"github.com/neprel/git-hint/internal/evals/runner"
)

const controlledRunTotal = 900 * time.Second
const controlledRunCleanup = 300 * time.Second

var errControlledRun = errors.New("controlled capture requires one eval, OpenCode, one attempt, --backend-expect, --study-launch single-root-v1, --ripgrep-binary and --opencode-tgz; variants are not supported")
var errBackendFile = errors.New("backend expectation file is invalid or exceeds its limit")

type controlledEvalRun struct {
	backend, study, ripgrep string
	data                    []byte
	enabled                 bool
}

func controlledEvalRunFlags(fs *flag.FlagSet) *controlledEvalRun {
	c := &controlledEvalRun{}
	fs.StringVar(&c.backend, "backend-expect", "", "complete backend metadata JSON; compare the normal start capture before launch (uses SSH)")
	fs.StringVar(&c.study, "study-launch", "", "controlled OpenCode launch: single-root-v1 (captures the prelaunch tree)")
	fs.StringVar(&c.ripgrep, "ripgrep-binary", "", "controlled launch: local pinned ripgrep binary for the resolved container target")
	return c
}

func (c *controlledEvalRun) selected(fs *flag.FlagSet) bool {
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "backend-expect" || f.Name == "study-launch" || f.Name == "ripgrep-binary" {
			c.enabled = true
		}
	})
	return c.enabled
}

func (c *controlledEvalRun) prepare(ctx context.Context, dir string, o pipeline.SuiteOptions, variants bool) error {
	if !c.enabled {
		return nil
	}
	if c.backend == "" || c.study != string(runner.StudyLaunchSingleRootV1) || c.ripgrep == "" || o.OpenCodeTgz == "" ||
		o.Agent != "opencode" || o.Attempts != 1 || len(o.Evals) != 1 || !slices.Contains(pipeline.Arms, o.Arm) || variants {
		return errControlledRun
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	path := c.backend
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	// Nonblocking/no-follow open also refuses a substituted FIFO or final symlink.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errBackendFile
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > runner.MaxBackendExpectationBytes {
		return errBackendFile
	}
	data, err := io.ReadAll(io.LimitReader(f, runner.MaxBackendExpectationBytes+1))
	if err != nil || len(data) > runner.MaxBackendExpectationBytes {
		return errBackendFile
	}
	after, err := f.Stat()
	current, cerr := os.Lstat(path)
	if err != nil || cerr != nil || !current.Mode().IsRegular() || !os.SameFile(before, current) ||
		after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		return errBackendFile
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.data = data
	return nil
}

func (c *controlledEvalRun) apply(o *pipeline.SuiteOptions, key []byte) error {
	if !c.enabled {
		return nil
	}
	expect, err := runner.ParseBackendExpectation(c.data, key)
	if err != nil {
		return err
	}
	o.StudyLaunchPolicy, o.CapturePrelaunchTree = runner.StudyLaunchSingleRootV1, true
	o.RipgrepBinary, o.BackendCapture, o.ExpectedBackend = c.ripgrep, expect.Capture, expect.Snapshot
	return nil
}

// The original entry time anchors both deadlines. Work cancellation leaves the
// existing runner's bounded teardown and proxy close a 300-second allowance.
// Synchronous filesystem/SDK calls are cooperative, not an OS process watchdog.
func controlledEvalContext(entered time.Time) (context.Context, context.Context, context.CancelFunc) {
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	total, cancelTotal := context.WithDeadline(parent, entered.Add(controlledRunTotal))
	work, cancelWork := context.WithDeadline(total, entered.Add(controlledRunTotal-controlledRunCleanup))
	return work, total, func() { cancelWork(); cancelTotal(); stop() }
}
