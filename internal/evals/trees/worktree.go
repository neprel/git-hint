package trees

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neprel/git-hint/internal/evals/library"
	"github.com/neprel/git-hint/internal/readgit"
)

// CapturePolicy is an explicit reader contract, not catalogue or completion authority.
type CapturePolicy string

// StrictCapturePolicyV1 opts into the regular-file, protected-path, bounded reader.
// It covers Git-selected paths only; Git may omit untracked special files. No whole-
// worktree absence, catalogue, repeated-capture or runtime authority follows.
const StrictCapturePolicyV1 CapturePolicy = "strict-worktree-reader-v1"
const strictInventoryBytes int64 = 4 << 20
const legacyInventoryBytes int64 = 32 << 20
const strictCaptureSeconds = 30 * time.Second

// FromWorktreeWithCapturePolicy uses the same selection and reader as FromWorktree.
// Zero bounds select 8 MiB/file, 64 MiB/tree and 10,000 selected paths. Positive
// bounds tighten those maxima; negative bounds refuse. The earlier caller deadline wins
// over 30 seconds. Git stdout is at most 4 MiB/command and stderr at most 64 KiB.
func FromWorktreeWithCapturePolicy(ctx context.Context, l *library.Library, root string, lim Limits, policy CapturePolicy) (string, error) {
	if policy != StrictCapturePolicyV1 {
		return "", unsupported("capture_policy_unknown", "the capture reader policy is unsupported")
	}
	lim, e := strictEffectiveLimits(lim)
	if e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(ctx, strictCaptureSeconds)
	defer cancel()
	return captureWorktree(ctx, l, root, lim, true)
}

// strictEffectiveLimits does not depend on mutable legacy DefaultLimits.
func strictEffectiveLimits(lim Limits) (Limits, error) {
	if lim.MaxFileBytes < 0 || lim.MaxTotalBytes < 0 || lim.MaxFiles < 0 {
		return Limits{}, unsupported("capture_limits_invalid", "strict capture bounds must be zero-default or positive")
	}
	if lim.MaxFileBytes == 0 || lim.MaxFileBytes > 8<<20 {
		lim.MaxFileBytes = 8 << 20
	}
	if lim.MaxTotalBytes == 0 || lim.MaxTotalBytes > 64<<20 {
		lim.MaxTotalBytes = 64 << 20
	}
	if lim.MaxFiles == 0 || lim.MaxFiles > 10000 {
		lim.MaxFiles = 10000
	}
	return lim, nil
}

// FromWorktree stores Git-visible tracked and untracked nonignored files. Legacy final
// symlinks remain target-byte objects; every regular file uses the shared bounded reader.
func FromWorktree(ctx context.Context, l *library.Library, root string, lim Limits) (string, error) {
	return captureWorktree(ctx, l, root, lim.filled(), false)
}

func captureWorktree(ctx context.Context, l *library.Library, root string, lim Limits, strict bool) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	source, e := openWorktreeRoot(root, strict)
	if e != nil {
		return "", e
	}
	defer source.close()
	return captureWorktreeSource(ctx, l, source, lim, strict, nil)
}

// The sole selection/read/store loop. Legacy callers do not collect receipts.
func captureWorktreeSource(ctx context.Context, l *library.Library, source *worktreeRoot, lim Limits, strict bool, receipt *receiptCapture) (string, error) {
	cap := legacyInventoryBytes
	if strict {
		cap = strictInventoryBytes
	}
	if receipt != nil && receipt.observer != nil {
		if e := receipt.observer.beforeInventory(ctx); e != nil {
			return "", e
		}
	}
	out, e := source.git(ctx, cap, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate")
	if e != nil {
		return "", unsupported("inventory_unavailable", "the bounded Git inventory failed")
	}
	sub, e := source.git(ctx, cap, "ls-files", "-z", "--stage")
	if e != nil && strict {
		return "", unsupported("inventory_unavailable", "the bounded Git stage inventory failed")
	}
	if receipt != nil {
		if e := receipt.selection(out, lim.MaxFiles); e != nil {
			return "", e
		}
		if e := receipt.stages(sub); e != nil {
			return "", e
		}
	}
	if e == nil {
		for _, rec := range bytes.Split(sub, []byte{0}) {
			if strict && len(rec) > 0 && !validStageRecord(rec) {
				return "", unsupported("inventory_unavailable", "the Git stage inventory is malformed")
			}
			if bytes.HasPrefix(rec, []byte("160000 ")) {
				return "", unsupported("submodule_unsupported", "the repository has a submodule")
			}
		}
	}
	if len(out) > 0 && out[len(out)-1] != 0 {
		return "", unsupported("inventory_unavailable", "the Git inventory is incomplete")
	}
	if strict && len(sub) > 0 && sub[len(sub)-1] != 0 {
		return "", unsupported("inventory_unavailable", "the Git stage inventory is incomplete")
	}
	if receipt != nil {
		if receipt.afterInventory != nil {
			receipt.afterInventory()
		}
		if receipt.observer != nil {
			paths := make([]string, 0, len(receipt.indexed))
			for _, p := range bytes.Split(out, []byte{0}) {
				if len(p) > 0 {
					paths = append(paths, string(p))
				}
			}
			if e := receipt.observer.afterInventory(ctx, paths, append([]IndexStageFact(nil), receipt.value.Stages...)); e != nil {
				return "", e
			}
		}
	}
	var files []File
	var total int64
	seen := map[string]bool{}
	for _, raw := range bytes.Split(out, []byte{0}) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		p := string(raw)
		if p == "" {
			continue
		}
		if !validPath(p) || seen[p] {
			return "", unsupported("baseline_missing", "unexpected path form in the work tree")
		}
		if len(seen) >= lim.MaxFiles {
			return "", unsupported("binary_too_large", "the inventory exceeds the path count limit")
		}
		seen[p] = true
		if receipt != nil && !utf8.ValidString(p) {
			return "", receiptProblem("receipt_path_invalid")
		}
		if strict && strictProtectedPath(p) {
			return "", unsupported("protected_path", "the inventory names a protected source path")
		}
		remaining := lim.MaxTotalBytes - total
		bound := lim.MaxFileBytes
		if remaining < bound {
			bound = remaining
		}
		hooks := readHooks{}
		if receipt != nil {
			hooks = receipt.readHooks
			hooks.observe = true
		}
		entry, e := source.read(ctx, p, bound, strict, hooks)
		if e != nil {
			return "", e
		}
		if entry.absent || entry.skip {
			if receipt != nil {
				if entry.skip {
					return "", unsupported("receipt_invalid", "strict source skipped an entry")
				}
				receipt.absent(p, entry.absenceIdentity)
			}
			continue
		}
		if bytes.HasPrefix(entry.data, lfsPointer) {
			return "", unsupported("lfs_unsupported", "the repository holds Git LFS pointers")
		}
		if len(files) >= lim.MaxFiles {
			return "", unsupported("binary_too_large", "the state exceeds the tree count limit")
		}
		total += int64(len(entry.data))
		if total > lim.MaxTotalBytes {
			return "", unsupported("binary_too_large", "the state exceeds the tree byte limit")
		}
		d, e := l.PutObjectContext(ctx, entry.data)
		if e != nil {
			return "", e
		}
		files = append(files, File{Path: p, Mode: entry.mode, Size: int64(len(entry.data)), Digest: d})
		if receipt != nil {
			receipt.present(p, entry, d)
		}
	}
	if receipt != nil && receipt.observer != nil {
		if e := receipt.observer.afterContent(ctx, receipt.value); e != nil {
			return "", e
		}
	}
	if e = source.verify(); e != nil {
		return "", e
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	digest, e := putTreeContext(ctx, l, files)
	if e != nil {
		return "", e
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return digest, nil
}

type readHooks struct {
	beforeOpen, afterOpen, afterRead, beforeAbsenceVerify func()
	observe                                               bool
}
type worktreeEntry struct {
	data                          []byte
	mode                          string
	absent, skip                  bool
	pathIdentity, absenceIdentity string
}

// readRegular is the one bounded same-descriptor content core. Platform source readers
// supply descriptor/path identity checks; tests can deterministically mutate between them.
func readRegular(ctx context.Context, f *os.File, limit int64, hooks readHooks) ([]byte, error) {
	if limit < 0 {
		return nil, unsupported("binary_too_large", "the remaining tree budget is exhausted")
	}
	if hooks.afterOpen != nil {
		hooks.afterOpen()
	}
	var out []byte
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		want := int64(len(buf))
		remaining := limit - int64(len(out))
		if remaining < want {
			want = remaining + 1
		}
		n, e := f.Read(buf[:int(want)])
		if int64(len(out)+n) > limit {
			return nil, unsupported("binary_too_large", "a file exceeds its read bound")
		}
		out = append(out, buf[:n]...)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
	}
	if hooks.afterRead != nil {
		hooks.afterRead()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, nil
}

func (s *worktreeRoot) git(ctx context.Context, limit int64, args ...string) ([]byte, error) {
	if e := s.verify(); e != nil {
		return nil, e
	}
	dir, _ := s.gitDirectory()
	var out []byte
	var e error
	observer, _ := ctx.Value(sourceViewDispatchKey{}).(sourceViewDispatcher)
	if observer != nil {
		out, e = observer.sourceOutput(ctx, dir, limit, args...)
	} else {
		out, e = readgit.Output(ctx, dir, limit, args...)
	}
	if e != nil && !(observer != nil && readgit.ExitOne(e)) {
		return nil, unsupported("inventory_unavailable", "the bounded Git command failed")
	}
	if verifyErr := s.verify(); verifyErr != nil {
		return nil, verifyErr
	}
	// Only typed source queries distinguish a complete nonmatch (exit 1).
	// Each source caller must explicitly admit that outcome. Legacy callers
	// retain the original fail-closed error mapping.
	return out, e
}

var stageObjectHash = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func validStageRecord(rec []byte) bool {
	head, p, ok := strings.Cut(string(rec), "\t")
	if !ok || !validPath(p) {
		return false
	}
	fields := strings.Fields(head)
	if len(fields) != 3 || !stageObjectHash.MatchString(fields[1]) {
		return false
	}
	switch fields[0] {
	case "100644", "100755", "120000", "160000":
	default:
		return false
	}
	return fields[2] == "0" || fields[2] == "1" || fields[2] == "2" || fields[2] == "3"
}

// Strict protection is conservative across case-sensitive and case-insensitive volumes.
// Legacy validPath/tree spelling remains unchanged. Git metadata is forbidden at every
// component (as validPath already does for exact .git); .hint protects the root store only.
func strictProtectedPath(p string) bool {
	for i, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") || i == 0 && strings.EqualFold(part, ".hint") {
			return true
		}
	}
	return false
}
