package trees

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

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
	cap := legacyInventoryBytes
	if strict {
		cap = strictInventoryBytes
	}
	out, e := source.git(ctx, cap, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate")
	if e != nil {
		return "", unsupported("inventory_unavailable", "the bounded Git inventory failed")
	}
	sub, e := source.git(ctx, cap, "ls-files", "-z", "--stage")
	if e != nil && strict {
		return "", unsupported("inventory_unavailable", "the bounded Git stage inventory failed")
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
		if strict && strictProtectedPath(p) {
			return "", unsupported("protected_path", "the inventory names a protected source path")
		}
		remaining := lim.MaxTotalBytes - total
		bound := lim.MaxFileBytes
		if remaining < bound {
			bound = remaining
		}
		entry, e := source.read(ctx, p, bound, strict, readHooks{})
		if e != nil {
			return "", e
		}
		if entry.absent || entry.skip {
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

type readHooks struct{ beforeOpen, afterOpen, afterRead func() }
type worktreeEntry struct {
	data         []byte
	mode         string
	absent, skip bool
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
	out, e := readgit.Output(ctx, dir, limit, args...)
	if e != nil {
		return nil, unsupported("inventory_unavailable", "the bounded Git command failed")
	}
	if e = s.verify(); e != nil {
		return nil, e
	}
	return out, nil
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
