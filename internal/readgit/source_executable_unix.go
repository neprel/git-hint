//go:build unix

package readgit

// Executable-only pinning. Configuration/source content uses the trees reader;
// this helper never discovers, parses or reads configuration or a project store.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"reflect"
	"strings"
)

type executableNode struct {
	Dev, Ino               uint64
	Mode                   uint32
	Size, MT, MTN, CT, CTN int64
}

func executableStat(fd int) (executableNode, error) {
	var s unix.Stat_t
	e := unix.Fstat(fd, &s)
	return executableNode{uint64(s.Dev), s.Ino, uint32(s.Mode), s.Size, s.Mtim.Sec, s.Mtim.Nsec, s.Ctim.Sec, s.Ctim.Nsec}, e
}
func executableParent(path string) (int, string, []string, error) {
	if !canonicalSourcePath(path) {
		return -1, "", nil, errSourceInvocation
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return -1, "", nil, errSourceInvocation
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var nodes []string
	for _, p := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, "", nil, errSourceInvocation
		}
		fd = next
		n, e := executableStat(fd)
		if e != nil {
			unix.Close(fd)
			return -1, "", nil, errSourceInvocation
		}
		nodes = append(nodes, fmt.Sprintf("%d:%d:%d", n.Dev, n.Ino, n.Mode))
	}
	return fd, parts[len(parts)-1], nodes, nil
}
func inspectSourceExecutable(ctx context.Context, path string) (sourceExecutableIdentity, int64, error) {
	var zero sourceExecutableIdentity
	if e := ctx.Err(); e != nil {
		return zero, 0, e
	}
	parent, name, chain, e := executableParent(path)
	if e != nil {
		return zero, 0, e
	}
	defer unix.Close(parent)
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return zero, 0, errSourceInvocation
	}
	f := os.NewFile(uintptr(fd), "git-source-executable")
	defer f.Close()
	before, e := executableStat(fd)
	if e != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Mode&0111 == 0 || before.Size > 64<<20 {
		return zero, 0, errSourceInvocation
	}
	hash := sha256.New()
	buf := make([]byte, 32<<10)
	var used int64
	for {
		if e = ctx.Err(); e != nil {
			return zero, used, e
		}
		n, re := f.Read(buf)
		used += int64(n)
		if used > 64<<20 {
			return zero, used, errSourceInvocation
		}
		hash.Write(buf[:n])
		if re == io.EOF {
			break
		}
		if re != nil || n == 0 {
			return zero, used, errSourceInvocation
		}
	}
	after, e := executableStat(fd)
	if e != nil || before != after || used != before.Size {
		return zero, used, errSourceInvocation
	}
	p, n, endChain, e := executableParent(path)
	if e != nil {
		return zero, used, e
	}
	defer unix.Close(p)
	named, e := unix.Openat(p, n, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return zero, used, errSourceInvocation
	}
	defer unix.Close(named)
	actual, e := executableStat(named)
	if e != nil || actual != after || !reflect.DeepEqual(chain, endChain) {
		return zero, used, errSourceInvocation
	}
	if e = ctx.Err(); e != nil {
		return zero, used, e
	}
	node := fmt.Sprintf("git-executable-node-v1:%+v:%v", after, endChain)
	sum := sha256.Sum256([]byte(node))
	if e = f.Close(); e != nil {
		return zero, used, errSourceInvocation
	}
	return sourceExecutableIdentity{Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Node: "sha256:" + hex.EncodeToString(sum[:]), Size: used}, used, nil
}
