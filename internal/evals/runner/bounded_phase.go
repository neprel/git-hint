package runner

// runBoundedPhase is the existing reference lifecycle, shared by internal B1
// machinery. It owns no policy admission and never renews the operation endpoint.
import (
	"context"
	"time"
)

func runBoundedPhase(ctx, cleanup context.Context, anchor time.Time, window [4]int64,
	duration, cleanupDuration time.Duration, archiveCap int64, role string,
	fn func(context.Context, *referencePhase) error) (*ReferencePhaseAudit, error) {
	work := anchor.Add(time.Duration(window[0]) * time.Millisecond)
	if earlier := time.Now().Add(duration); earlier.Before(work) {
		work = earlier
	}
	// Tighten all cleanup endpoints with the earlier work cap. No endpoint ever extends
	// the complete schedule sealed before the first engine request.
	stop := work.Add(cleanupDuration / 3)
	remove := work.Add(2 * cleanupDuration / 3)
	join := work.Add(cleanupDuration)
	x := &referencePhase{parent: cleanup, workEnd: work, stopEnd: stop, removeEnd: remove, joinEnd: join, anchor: anchor, archiveCap: archiveCap}
	x.audit = ReferencePhaseAudit{Role: role, StartedMS: time.Since(anchor).Milliseconds(), WorkEndMS: work.Sub(anchor).Milliseconds(), StopEndMS: stop.Sub(anchor).Milliseconds(), RemoveEndMS: remove.Sub(anchor).Milliseconds(), JoinEndMS: join.Sub(anchor).Milliseconds()}
	wc, cancel := context.WithDeadline(ctx, work)
	wc = context.WithValue(wc, referencePhaseKey{}, x)
	x.work = wc
	if ctx.Err() != nil || !time.Now().Before(work) {
		cancel()
		return nil, errReferencePrepare
	}
	er := fn(wc, x)
	if wc.Err() != nil || time.Now().After(work) {
		x.fail(errReferencePrepare)
	}
	if x.cid != "" && !x.removed {
		x.ensureCleanup()
	}
	cancel()
	x.audit.FinishedMS = time.Since(anchor).Milliseconds()
	if time.Now().After(join) {
		x.fail(errReferencePrepare)
	}
	if er != nil || x.err != nil {
		return &x.audit, errReferencePrepare
	}
	return &x.audit, nil
}
