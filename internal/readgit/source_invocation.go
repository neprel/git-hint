package readgit

// This immutable dispatch profile uses the narrowly scoped executable descriptor
// helper; source/config metadata stays in trees. No trusted source seal follows.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const SourceDispatchLimit = 88254

// Counts dispatch attempts, not independently observed process starts.
type SourceDispatchStats struct {
	Dispatches, ExecutableReads               int
	ExecutableBytes, StdoutBytes, StderrBytes int64
}
type sourceExecutableIdentity struct {
	Digest, Node string
	Size         int64
}
type SourceInvocation struct {
	path, match, identity string
	env                   []string
	executable            sourceExecutableIdentity
	mu                    sync.Mutex
	stats                 SourceDispatchStats
	failed                bool
}
type sourceInvocationKey struct{}

// WithSourceInvocation scopes one owned frozen dispatch path and environment.
func WithSourceInvocation(ctx context.Context, i *SourceInvocation) context.Context {
	return context.WithValue(ctx, sourceInvocationKey{}, i)
}
func sourceInvocationFrom(ctx context.Context) *SourceInvocation {
	i, _ := ctx.Value(sourceInvocationKey{}).(*SourceInvocation)
	return i
}
func (i *SourceInvocation) Stats() SourceDispatchStats {
	if i == nil {
		return SourceDispatchStats{}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.stats
}
func (i *SourceInvocation) ExecutableIdentity() (string, string) {
	if i == nil {
		return "", ""
	}
	return i.executable.Digest, i.executable.Node
}
func (i *SourceInvocation) recordOutput(out, err int64) {
	i.mu.Lock()
	i.stats.StdoutBytes += out
	i.stats.StderrBytes += err
	i.mu.Unlock()
}
func (i *SourceInvocation) admit(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failed || i.stats.Dispatches >= SourceDispatchLimit {
		return errSourceInvocation
	}
	id, n, e := inspectSourceExecutable(ctx, i.path)
	i.stats.ExecutableReads++
	i.stats.ExecutableBytes += n
	if e != nil || id != i.executable {
		i.failed = true
		if e != nil {
			return e
		}
		return errSourceInvocation
	}
	i.stats.Dispatches++
	return nil
}

func (i *SourceInvocation) Executable() string {
	if i == nil {
		return ""
	}
	return i.path
}
func (i *SourceInvocation) PATHMatch() string {
	if i == nil {
		return ""
	}
	return i.match
}
func (i *SourceInvocation) Identity() string {
	if i == nil {
		return ""
	}
	return i.identity
}
func (i *SourceInvocation) Environment() []string {
	if i == nil {
		return nil
	}
	return append([]string(nil), i.env...)
}

var errSourceInvocation = errors.New("git_source_invocation_invalid")

// FreezeSourceInvocation copies only the strict profile's local selectors. No
// credential/provider/model environment value is retained. It does not run Git.
func FreezeSourceInvocation(ctx context.Context, base []string) (*SourceInvocation, error) {
	if ctx == nil {
		return nil, errSourceInvocation
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	selected := map[string]string{}
	seen := map[string]bool{}
	if len(base) > 4096 {
		return nil, errSourceInvocation
	}
	for _, item := range base {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(k)
		if strings.HasPrefix(upper, "GIT_") {
			return nil, errSourceInvocation
		}
		if (strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_")) && v != "" {
			return nil, errSourceInvocation
		}
		switch k {
		case "PATH", "HOME", "XDG_CONFIG_HOME":
			if seen[k] || len(v) > 16384 || strings.ContainsAny(v, "\x00\r\n") {
				return nil, errSourceInvocation
			}
			seen[k] = true
			selected[k] = v
		}
	}
	home := selected["HOME"]
	if !canonicalSourcePath(home) {
		return nil, errSourceInvocation
	}
	if v := selected["XDG_CONFIG_HOME"]; v != "" && !canonicalSourcePath(v) {
		return nil, errSourceInvocation
	}
	path := selected["PATH"]
	if path == "" {
		return nil, errSourceInvocation
	}
	match := ""
	for _, dir := range filepath.SplitList(path) {
		if !canonicalSourcePath(dir) {
			return nil, errSourceInvocation
		}
		candidate := filepath.Join(dir, "git")
		fi, e := os.Stat(candidate)
		if e == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
			match = candidate
			break
		}
	}
	if match == "" {
		return nil, errSourceInvocation
	}
	resolved, e := filepath.EvalSymlinks(match)
	if e != nil || !canonicalSourcePath(resolved) {
		return nil, errSourceInvocation
	}
	fi, e := os.Stat(resolved)
	if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0111 == 0 {
		return nil, errSourceInvocation
	}
	env := []string{"PATH=" + path, "HOME=" + home, "LANG=C", "LC_ALL=C", "TZ=UTC", "GIT_OPTIONAL_LOCKS=0"}
	if v, ok := selected["XDG_CONFIG_HOME"]; ok {
		env = append(env, "XDG_CONFIG_HOME="+v)
	}
	sort.Strings(env)
	exe, n, e := inspectSourceExecutable(ctx, resolved)
	if e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(struct {
		Version, Match, Path string
		Env                  []string
		Executable           sourceExecutableIdentity
	}{"git-source-invocation-v1", match, resolved, env, exe})
	h := sha256.Sum256(raw)
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	return &SourceInvocation{path: resolved, match: match, identity: "sha256:" + hex.EncodeToString(h[:]), env: env, executable: exe, stats: SourceDispatchStats{ExecutableReads: 1, ExecutableBytes: n}}, nil
}
func canonicalSourcePath(p string) bool {
	return p != "" && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}
