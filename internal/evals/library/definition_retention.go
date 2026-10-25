package library

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/neprel/git-hint/internal/store"
)

// WorkDefinitionStateDir contains local immutable definition records. No public
// export/source authority is implied. Retention has no reviewed strong edges yet.
const WorkDefinitionStateDir = "work-definitions"
const ReferencePreparationStateDir = "reference-preparations"
const CapturePolicyStateDir = "capture-policies"
const CaptureReservationStateDir = "capture-source-reservations"
const CaptureSourceStateDir = "capture-sources"

var ErrDefinitionReferences = errors.New("destructive retention refused: unmapped work-definition references")

func (l *Library) definitionRetentionProblem() error {
	for _, dir := range []string{WorkDefinitionStateDir, ReferencePreparationStateDir, CapturePolicyStateDir, CaptureReservationStateDir, CaptureSourceStateDir} {
		_, e := os.Lstat(l.s.Path("state", dir))
		if !os.IsNotExist(e) {
			return ErrDefinitionReferences
		}
	}
	return nil
}

func (l *Library) destructiveStateLock() (*store.Lock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return l.s.TryLockStateContext(ctx)
}
