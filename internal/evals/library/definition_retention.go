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

var ErrDefinitionReferences = errors.New("destructive retention refused: unmapped work-definition references")

func (l *Library) definitionRetentionProblem() error {
	_, e := os.Lstat(l.s.Path("state", WorkDefinitionStateDir))
	if os.IsNotExist(e) {
		return nil
	}
	// Existence, unreadable/special/empty directories and errors all refuse. Never
	// enumerate/delete referents before the new reference mapping receives review.
	return ErrDefinitionReferences
}

func (l *Library) destructiveStateLock() (*store.Lock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return l.s.TryLockStateContext(ctx)
}
