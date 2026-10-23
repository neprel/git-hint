package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Outcomes of the agent container as the runner observed it (draft v0.2 ModelLedger.agent_run).
// Only exited gives a usable run interval; nothing here is the true process start or end.
const (
	AgentExited                = "exited"
	AgentExitedNonzero         = "exited_nonzero"
	AgentStoppedAfterTimeout   = "stopped_after_timeout"
	AgentStoppedAfterCancel    = "stopped_after_cancel"
	AgentStoppedAfterWaitError = "stopped_after_wait_error"
	AgentStopUnconfirmed       = "stop_unconfirmed"
	AgentStartUnconfirmed      = "start_unconfirmed"
	AgentRefusedBeforeLaunch   = "refused_before_launch"
)

// agentEngine is the part of the engine API that the agent phase uses (tests replace it).
type agentEngine interface {
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

// Bounds of the engine calls after the agent phase: the stop, the one confirming wait and the
// inspect of the engine times.
const (
	agentStopBound    = 30 * time.Second
	agentConfirmBound = 30 * time.Second
	agentInspectBound = 10 * time.Second
)

// runAgentEnclosed runs the agent container like runToEnd and records its launch/exit enclosure
// in the counter's ledger (when the execution digest is known): the start offset is read before
// ContainerStart is called (the agent can send requests before the reply returns), the end offset
// when the wait, or the stop and one confirming wait, has completed. A ContainerStart error is
// start_unconfirmed (the engine can have started the container); a timeout, cancellation or wait
// error is followed by a bounded stop and one bounded confirming wait, and stays stop_unconfirmed
// without that confirmation. The engine's own start and finish times are descriptive only. The
// returned error is the phase's error as runToEnd reports it.
func runAgentEnclosed(ctx context.Context, eng agentEngine, id string, limit time.Duration, c *Counter, digest string) error {
	run := agentRun{digest: digest}
	defer func() {
		if digest != "" {
			c.recordAgentRun(run)
		}
	}()
	start := c.mark()
	run.start = &start
	if _, err := eng.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		run.outcome = AgentStartUnconfirmed
		run.engineStarted, run.engineFinished = engineTimes(eng, id)
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	w := eng.ContainerWait(wctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var runErr error
	select {
	case res := <-w.Result:
		if res.Error != nil && res.Error.Message != "" {
			run.outcome, runErr = AgentStoppedAfterWaitError, fmt.Errorf("agent container: %s", res.Error.Message)
			break
		}
		end := c.mark()
		run.end = &end
		code := int(res.StatusCode)
		run.exitCode = &code
		run.outcome = AgentExited
		if res.StatusCode != 0 {
			run.outcome, runErr = AgentExitedNonzero, exitError{"agent", res.StatusCode}
		}
	case err := <-w.Error:
		switch {
		case ctx.Err() != nil:
			run.outcome = AgentStoppedAfterCancel
		case wctx.Err() != nil:
			run.outcome = AgentStoppedAfterTimeout
		default:
			run.outcome = AgentStoppedAfterWaitError
		}
		runErr = err
		if wctx.Err() != nil {
			runErr = timeoutError{"agent"}
		}
	}
	if run.end == nil { // stopped: the end is the confirmed stop, or unknown
		if stopConfirmed(eng, id) {
			end := c.mark()
			run.end = &end
		} else {
			run.outcome = AgentStopUnconfirmed
		}
	}
	run.engineStarted, run.engineFinished = engineTimes(eng, id)
	return runErr
}

// stopConfirmed stops the container (bounded) and confirms with one bounded wait that it is not
// running; a stop error alone does not decide, the confirming wait does.
func stopConfirmed(eng agentEngine, id string) bool {
	t := 5
	sctx, scancel := context.WithTimeout(context.Background(), agentStopBound)
	_, _ = eng.ContainerStop(sctx, id, client.ContainerStopOptions{Timeout: &t})
	scancel()
	cctx, ccancel := context.WithTimeout(context.Background(), agentConfirmBound)
	defer ccancel()
	w := eng.ContainerWait(cctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-w.Result:
		return res.Error == nil || res.Error.Message == ""
	case <-w.Error:
		return false
	}
}

// engineTimes are the engine's reported start and finish times of the container (engine clock;
// descriptive, never compared with the runner's offsets), or nil when unset or unreadable.
func engineTimes(eng agentEngine, id string) (*string, *string) {
	ctx, cancel := context.WithTimeout(context.Background(), agentInspectBound)
	defer cancel()
	ins, err := eng.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || ins.Container.State == nil {
		return nil, nil
	}
	return engineTime(ins.Container.State.StartedAt), engineTime(ins.Container.State.FinishedAt)
}

func engineTime(s string) *string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 { // the engine writes year 1 for a time that never happened
		return nil
	}
	v := t.UTC().Format(time.RFC3339Nano)
	return &v
}
