// Command release builds the git-hint release archives. It is a maintainer tool; it is not
// part of the git-hint binary.
//
//	go run ./tools/release build  -version v0.1.0 -out dist   # archives, SHA256SUMS, manifest.json
//	go run ./tools/release check  <binary>...                 # forbidden content in a binary
//	go run ./tools/release verify -dir dist                   # checksums and manifest
//
// The build is pure Go (CGO_ENABLED=0), with -trimpath, an empty build ID and the commit
// time as the version date and the archive time, so the same commit and Go version give
// byte-identical archives.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// Targets are the published platforms. Windows is deferred and never built here.
var Targets = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}

// Files copied next to the binary in each archive.
var docFiles = []string{"LICENSE", "THIRD_PARTY_NOTICES.md", "README.md"}

const pkg = "github.com/neprel/git-hint/internal/version"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release build|check|verify ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = cmdBuild(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

// Manifest describes one release.
type Manifest struct {
	Name            string     `json:"name"`
	Version         string     `json:"version"`
	Commit          string     `json:"commit"`
	Date            string     `json:"date"`
	GoVersion       string     `json:"go_version"`
	StandardVersion string     `json:"standard_version"`
	Artifacts       []Artifact `json:"artifacts"`
	Deferred        []string   `json:"deferred_platforms"`
}

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// semver is "v" plus the version pattern of the HINT data standard (common.schema.json,
// producer version): a pre-release is alpha, beta, rc or dev with an optional number. The
// client writes its version into every document, so another pre-release tag would make
// every export invalid. TestVersionPatternMatchesSchema keeps the two in step.
var semver = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc|dev)(\.(0|[1-9][0-9]*))?)?$`)

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	ver := fs.String("version", "", "release tag, for example v0.1.0")
	out := fs.String("out", "dist", "output directory (must not exist)")
	targets := fs.String("targets", strings.Join(Targets, ","), "comma-separated os/arch list")
	dirty := fs.Bool("allow-dirty", false, "allow uncommitted changes (local tests only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !semver.MatchString(*ver) {
		return fmt.Errorf("version %q is not a SemVer tag like v0.1.0", *ver)
	}
	var plats [][2]string
	for _, t := range strings.Split(*targets, ",") {
		goos, arch, ok := strings.Cut(strings.TrimSpace(t), "/")
		if !ok || !slices.Contains(Targets, goos+"/"+arch) {
			return fmt.Errorf("target %q is not a release target (%s)", t, strings.Join(Targets, ", "))
		}
		plats = append(plats, [2]string{goos, arch})
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists", *out)
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if st, _ := git("status", "--porcelain", "--untracked-files=no"); st != "" && !*dirty {
		return errors.New("the work tree has uncommitted changes")
	}
	epoch, err := git("log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return err
	}
	var sec int64
	fmt.Sscan(epoch, &sec)
	when := time.Unix(sec, 0).UTC()
	std, err := goOutput("run", "./tools/release/stdversion")
	if err != nil {
		return err
	}
	m := Manifest{Name: "git-hint", Version: *ver, Commit: commit, Date: when.Format(time.RFC3339),
		GoVersion: runtime.Version(), StandardVersion: std, Deferred: []string{"windows"}}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "git-hint-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ldflags := strings.Join([]string{"-s", "-w", "-buildid=",
		"-X", pkg + ".Version=" + strings.TrimPrefix(*ver, "v"),
		"-X", pkg + ".Commit=" + commit,
		"-X", pkg + ".Date=" + m.Date}, " ")
	lic, err := licenseFiles()
	if err != nil {
		return err
	}
	for _, pl := range plats {
		goos, arch := pl[0], pl[1]
		t := goos + "/" + arch
		bin := filepath.Join(tmp, goos+"_"+arch, "git-hint")
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, "./cmd/git-hint")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+arch, "GOFLAGS=-mod=readonly")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s: %w", t, err)
		}
		if bad := checkBinary(bin); len(bad) > 0 {
			return fmt.Errorf("%s: forbidden content: %s", t, strings.Join(bad, "; "))
		}
		name := fmt.Sprintf("git-hint_%s_%s_%s", strings.TrimPrefix(*ver, "v"), goos, arch)
		file := name + ".tar.gz"
		if err := writeArchive(filepath.Join(*out, file), name, bin, lic, when); err != nil {
			return err
		}
		sum, size, err := hashFile(filepath.Join(*out, file))
		if err != nil {
			return err
		}
		m.Artifacts = append(m.Artifacts, Artifact{OS: goos, Arch: arch, File: file, SHA256: sum, Size: size})
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].File < m.Artifacts[j].File })
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	return writeSums(*out)
}

// writeArchive writes a deterministic tar.gz: fixed order, owner, modes and time.
func writeArchive(dst, dir, bin string, lic []namedFile, when time.Time) error {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	zw.ModTime = when
	tw := tar.NewWriter(zw)
	add := func(name string, mode int64, body []byte) error {
		h := &tar.Header{Name: dir + "/" + name, Mode: mode, Size: int64(len(body)), ModTime: when, Typeflag: tar.TypeReg,
			Uname: "root", Gname: "root", Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0o755, ModTime: when, Typeflag: tar.TypeDir,
		Uname: "root", Gname: "root", Format: tar.FormatPAX}); err != nil {
		return err
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	if err := add("git-hint", 0o755, b); err != nil {
		return err
	}
	for _, f := range docFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := add(f, 0o644, b); err != nil {
			return err
		}
	}
	for _, f := range lic {
		if err := add(f.name, 0o644, f.body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

type namedFile struct {
	name string
	body []byte
}

// licenseFiles returns the license texts of the Go toolchain and of every module that the
// git-hint binary links, as licenses/<module path>/<file>, sorted. A module without a
// license file stops the build.
func licenseFiles() ([]namedFile, error) {
	goroot, err := goOutput("env", "GOROOT")
	if err != nil {
		return nil, err
	}
	var out []namedFile
	for _, n := range []string{"LICENSE", "PATENTS"} {
		// some packaged toolchains keep the files one level above GOROOT
		b, err := os.ReadFile(filepath.Join(goroot, n))
		if err != nil {
			if b, err = os.ReadFile(filepath.Join(goroot, "..", n)); err != nil {
				return nil, fmt.Errorf("Go toolchain %s not found", n)
			}
		}
		out = append(out, namedFile{"licenses/go/" + n, b})
	}
	list, err := goOutput("list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}", "./cmd/git-hint")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(list, "\n") {
		mod, dir, ok := strings.Cut(line, " ")
		if !ok || seen[mod] {
			continue
		}
		seen[mod] = true
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range ents {
			n := strings.ToUpper(e.Name())
			if !e.IsDir() && (strings.HasPrefix(n, "LICENSE") || strings.HasPrefix(n, "COPYING") || strings.HasPrefix(n, "NOTICE")) {
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return nil, err
				}
				out = append(out, namedFile{"licenses/" + mod + "/" + e.Name(), b})
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("module %s has no license file", mod)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func writeSums(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var lines []string
	for _, e := range ents {
		if e.IsDir() || e.Name() == "SHA256SUMS" {
			continue
		}
		sum, _, err := hashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		lines = append(lines, sum+"  "+e.Name())
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i][66:] < lines[j][66:] })
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "dist", "release directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(*dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || strings.Contains(name, "/") {
			return fmt.Errorf("bad line %q", line)
		}
		got, _, err := hashFile(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		if got != sum {
			return fmt.Errorf("%s: checksum mismatch", name)
		}
		n++
	}
	var m Manifest
	mb, err := os.ReadFile(filepath.Join(*dir, "manifest.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		return err
	}
	for _, a := range m.Artifacts {
		got, size, err := hashFile(filepath.Join(*dir, a.File))
		if err != nil || got != a.SHA256 || size != a.Size || a.OS == "windows" {
			return fmt.Errorf("%s: manifest entry does not match", a.File)
		}
	}
	fmt.Printf("%d files verified, %d artifacts in the manifest\n", n, len(m.Artifacts))
	return nil
}

// forbidden content of a release binary: test fixture content, local home paths, private
// IPv4 addresses and common credential prefixes. The bare canary markers ("HINTCANARY_",
// "canary_dir_e1d2") are expected: the export validator embeds them to reject documents
// that carry them (internal/hint/semantic.go). Fixture content uses marker suffixes.
var forbidden = []struct {
	name string
	re   *regexp.Regexp
}{
	{"test fixture content", regexp.MustCompile(`HINTCANARY_(?:SRC|FILE|MATCH|OLD|NEW|PATCH|CMD|PATTERN)|canary_dir_e1d2/[a-z]`)},
	{"home path", regexp.MustCompile(`/Users/[A-Za-z0-9._-]+/|/home/[a-z][a-z0-9._-]*/`)},
	{"private IPv4", regexp.MustCompile(`\b(?:10\.\d{1,3}|192\.168|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}\b`)},
	{"credential", regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|xox[abp]-[A-Za-z0-9-]{10,}`)},
	{"source fixture", regexp.MustCompile(`package billing|func Rate\(`)},
}

func checkBinary(p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return []string{err.Error()}
	}
	var bad []string
	for _, f := range forbidden {
		if m := f.re.Find(b); m != nil {
			bad = append(bad, f.name)
		}
	}
	return bad
}

func cmdCheck(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: release check <binary>...")
	}
	failed := false
	for _, p := range args {
		if bad := checkBinary(p); len(bad) > 0 {
			fmt.Printf("%s: FORBIDDEN %s\n", p, strings.Join(bad, ", "))
			failed = true
		} else {
			fmt.Printf("%s: ok\n", p)
		}
	}
	if failed {
		return errors.New("forbidden content found")
	}
	return nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func goOutput(args ...string) (string, error) {
	out, err := exec.Command("go", args...).Output()
	return strings.TrimSpace(string(out)), err
}
