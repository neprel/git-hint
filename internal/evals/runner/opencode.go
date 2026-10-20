package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Pinned OpenCode in the container runner (07B-J §22, approved with conditions). Nothing is
// fetched in the agent container: the runner takes the pinned archive of the attempt's
// target from its verified cache, from a local file, or from the npm registry (only during an
// explicit controlled run that needs it), checks its SHA-512 pin, extracts exactly one
// regular package/bin/opencode in memory, and copies it into the agent container before the
// start, owned by root with mode 0755. The agent runs it with a fixed argv and a config that
// names the gate and a dummy key.

// DockerOpenCodeVersion is the OpenCode version the container runner installs (v2, pinned
// below).
const DockerOpenCodeVersion = "2.0.26"

// OpenCodePins are the SHA-512 values (hex) of cli-<target>-<version>.tgz from the npm
// registry's dist.integrity.
var OpenCodePins = map[string]map[string]string{
	"2.0.22": {
		"linux-x64":        "0e5575aa01030e756aa5358c3eabfbb421c2717a1dcd904568c7318ec898758e84e601c7d90ebc25f6ac564b324359eeea6d7cf3b59028684eb1ea44fa82b263",
		"linux-arm64":      "b7fcaebbd0ea77f338e1d147f796e986eee290ee85013310b9593ff754019c2333b0ff3356b5d4ef30310f5c152e9618503a88a271268bcd17113945ab1edc87",
		"linux-x64-musl":   "24b935ad01c2e8bbf59c067962fabdbe968e62c7b0c810963aecd094493f5f072766dd1f1a1eaf7cf3e6261bc1873f5524a0bf6c2e70375dc4b8eab80a67b092",
		"linux-arm64-musl": "0a111ed68e7275464effb37a2c185ba7a4d21b87311088a5f517a3bfc7d33f188a142054bbc137c1a973a411804aec2f15daf888497667f593a5b5876923c8f8",
	},
	"2.0.26": {
		"linux-x64":        "508036ff5224f0768de780b8c69ec7f8e1e045fabdf795d48b1ad7fa42e616cf043e7d5f01c9a7dcea2237eddcdd57378c457f2c79a3d02cb548bea75b8c1d1b",
		"linux-arm64":      "206df5e88f3056a9dda21cc502858d787f3778c03cb0a40546d31eba4d967e0e0353585a092833009cf7fe06f2b63c21e9b5dd591ed5dc5092adf8e8ada28383",
		"linux-x64-musl":   "29fbcbbda70bc81d6849925180ebe2af984811f83606ca66f85dd2129add062cc18de9b9be8310ced88e608f4444b237d1ff6e14c81741629e994fe364aa6d5c",
		"linux-arm64-musl": "51ccd0f35593122a633209cb372f6e5c254b9b01e3465731fefb02ce14e97b1fa51dfbc0e434351ecfba0e32d61fd5eda3edf16a658c211da056cb6a2471b151",
	},
}

// Bounds of the OpenCode archive and binary (variables so tests can lower them).
var (
	maxOpenCodeTgz    int64 = 200 << 20
	maxOpenCodeBinary int64 = 400 << 20
	openCodeFetchTime       = 10 * time.Minute
)

// OpenCodeSource is where a controlled run takes the pinned OpenCode from.
type OpenCodeSource struct {
	Version  string
	CacheDir string // private directory of verified archives (digest-keyed)
	TgzFile  string // offline: a local archive, checked like a download
	Registry string // the registry base URL; empty: https://registry.npmjs.org
}

// openCodeBinary is a verified, extracted OpenCode binary with its provenance.
type openCodeBinary struct {
	data []byte
	prov map[string]any
}

func sha512Hex(b []byte) string { s := sha512.Sum512(b); return hex.EncodeToString(s[:]) }

// readBounded reads at most max bytes; more is an error.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("too large")
	}
	return b, nil
}

// archive returns the verified archive of one target: the cache entry if its hash still
// matches the pin, else the local file or the registry download, checked against the pin and
// written to the cache atomically.
func (s OpenCodeSource) archive(ctx context.Context, target string) ([]byte, string, error) {
	pin := OpenCodePins[s.Version][target]
	if pin == "" {
		return nil, "", fmt.Errorf("OpenCode %s has no pin for %s", s.Version, target)
	}
	cached := filepath.Join(s.CacheDir, pin+".tgz")
	if f, err := os.Open(cached); err == nil {
		b, err := readBounded(f, maxOpenCodeTgz)
		f.Close()
		if err == nil && sha512Hex(b) == pin {
			return b, "cache", nil
		}
		os.Remove(cached) // corrupted: replaced only by the same pinned artifact below
	}
	var b []byte
	var source string
	if s.TgzFile != "" {
		f, err := os.Open(s.TgzFile)
		if err != nil {
			return nil, "", err
		}
		b, err = readBounded(f, maxOpenCodeTgz)
		f.Close()
		if err != nil {
			return nil, "", fmt.Errorf("the OpenCode archive: %v", err)
		}
		source = "file"
	} else {
		var err error
		if b, err = s.fetch(ctx, target); err != nil {
			return nil, "", err
		}
		source = "registry"
	}
	if sha512Hex(b) != pin {
		return nil, "", errors.New("the OpenCode archive does not match its pin")
	}
	if err := os.MkdirAll(s.CacheDir, 0o700); err != nil {
		return nil, "", err
	}
	tmp, err := os.CreateTemp(s.CacheDir, ".tgz-")
	if err != nil {
		return nil, "", err
	}
	_, werr := tmp.Write(b)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp.Name())
		return nil, "", err
	}
	if err := os.Rename(tmp.Name(), cached); err != nil {
		os.Remove(tmp.Name())
		return nil, "", err
	}
	return b, source, nil
}

// fetch downloads the pinned archive: one exact URL, no redirects, bounded time and size.
func (s OpenCodeSource) fetch(ctx context.Context, target string) ([]byte, error) {
	base := s.Registry
	if base == "" {
		base = "https://registry.npmjs.org"
	}
	u := base + "/@opencode/cli-" + target + "/-/cli-" + target + "-" + s.Version + ".tgz"
	ctx, cancel := context.WithTimeout(ctx, openCodeFetchTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are refused") }}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the OpenCode download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the OpenCode download failed: status %d", resp.StatusCode)
	}
	b, err := readBounded(resp.Body, maxOpenCodeTgz)
	if err != nil {
		return nil, fmt.Errorf("the OpenCode download: %v", err)
	}
	return b, nil
}

// extractOpenCode returns the one regular package/bin/opencode of a verified archive, read
// in memory. Any absolute or escaping name, a duplicate of that path, or a link on its path
// is refused.
func extractOpenCode(tgz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	var bin []byte
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		clean := path.Clean(name)
		if path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || clean != name {
			return nil, fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		switch clean {
		case "package", "package/bin":
			if h.Typeflag != tar.TypeDir {
				return nil, fmt.Errorf("%s is not a directory", clean)
			}
		case "package/bin/opencode":
			if found {
				return nil, errors.New("package/bin/opencode appears twice")
			}
			if h.Typeflag != tar.TypeReg {
				return nil, errors.New("package/bin/opencode is not a regular file")
			}
			if bin, err = readBounded(tr, maxOpenCodeBinary); err != nil {
				return nil, fmt.Errorf("the OpenCode binary: %v", err)
			}
			found = true
		}
	}
	if !found {
		return nil, errors.New("the archive has no package/bin/opencode")
	}
	return bin, nil
}

// probeOutput runs a fixed argv in a clean probe container of an image (user 65534, no
// network, no capabilities, 30 s) and returns its complete output (stdout and stderr, at most
// max bytes) and its exit status; anything incomplete is an error.
func (d *Docker) probeOutput(ctx context.Context, img string, argv []string, max int) (string, int64, error) {
	pids := int64(64)
	id, err := d.create(ctx, &container.Config{Image: img, Cmd: argv, Entrypoint: []string{}, User: "65534:65534", WorkingDir: "/"},
		&container.HostConfig{NetworkMode: "none", CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			Resources: container.Resources{Memory: 256 << 20, PidsLimit: &pids}})
	if err != nil {
		return "", 0, err
	}
	defer d.remove(id)
	var status int64
	if err := d.runToEnd(ctx, id, 30*time.Second, "target probe"); err != nil {
		var ee exitError
		if !errors.As(err, &ee) {
			return "", 0, err
		}
		status = ee.code
	}
	logs, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", 0, err
	}
	defer logs.Close()
	b := &capWriter{max: max}
	if err := demux(b, b, logs); err != nil || b.over {
		return "", 0, errors.New("incomplete probe output")
	}
	return b.buf.String(), status, nil
}

var glibcEvidence = regexp.MustCompile(`(?i)\b(GLIBC|GNU libc)\b`)

// openCodeTarget determines the attempt's OpenCode target from the engine and the task's
// agent image: the image must be linux on the engine's own architecture (no emulation); the
// libc needs positive evidence (glibc or musl from ldd); x64 needs AVX2 (the baseline build
// is not pinned). Anything unknown is refused.
func (d *Docker) openCodeTarget(ctx context.Context, img string) (string, map[string]any, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", nil, err
	}
	arch := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "x64", "amd64": "x64"}[info.Info.Architecture]
	im, err := d.resolveImage(ctx, img)
	if err != nil {
		return "", nil, err
	}
	imArch := map[string]string{"arm64": "arm64", "amd64": "x64"}[im.Arch]
	rec := map[string]any{"engine_architecture": info.Info.Architecture, "image_os": im.OS, "image_architecture": im.Arch}
	if info.Info.OSType != "linux" || im.OS != "linux" || arch == "" || imArch != arch {
		return "", rec, errors.New("the agent image is not linux on the engine's own architecture")
	}
	out, _, err := d.probeOutput(ctx, img, []string{"ldd", "--version"}, 16<<10)
	if err != nil {
		return "", rec, fmt.Errorf("libc probe: %v", err)
	}
	libc := ""
	switch {
	case strings.Contains(strings.ToLower(out), "musl"):
		libc = "musl"
	case glibcEvidence.MatchString(out):
		libc = "glibc"
	default:
		return "", rec, errors.New("the libc of the agent image is unknown")
	}
	rec["libc"] = libc
	if arch == "x64" {
		cpu, _, err := d.probeOutput(ctx, img, []string{"cat", "/proc/cpuinfo"}, 4<<20)
		if err != nil || !regexp.MustCompile(`(?m)^flags\s*:.*\bavx2\b`).MatchString(cpu) {
			return "", rec, errors.New("AVX2 is not shown on this CPU (the baseline build is not pinned)")
		}
		rec["avx2"] = true
	}
	target := "linux-" + arch
	if libc == "musl" {
		target += "-musl"
	}
	rec["target"] = target
	return target, rec, nil
}

// openCodeBinaryFor resolves the target and returns the verified binary with provenance.
func (d *Docker) openCodeBinaryFor(ctx context.Context, src OpenCodeSource, img string) (*openCodeBinary, error) {
	target, rec, err := d.openCodeTarget(ctx, img)
	if err != nil {
		return nil, err
	}
	tgz, source, err := src.archive(ctx, target)
	if err != nil {
		return nil, err
	}
	bin, err := extractOpenCode(tgz)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bin)
	rec["version"], rec["tgz_sha512"], rec["binary_sha256"], rec["source"] = src.Version, sha512Hex(tgz), "sha256:"+hex.EncodeToString(sum[:]), source
	return &openCodeBinary{data: bin, prov: rec}, nil
}

var openCodeModel = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// openCodeFiles is the archive copied into the agent container before the start: the binary
// (root, 0755) and the config (owner 65534) that names the gate and a dummy key.
func openCodeFiles(bin []byte, c *Counter, model string) ([]byte, error) {
	if !openCodeModel.MatchString(model) {
		return nil, errors.New("unexpected model name")
	}
	cfg, _ := json.Marshal(map[string]any{
		"provider": map[string]any{"hint": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "hint",
			"options": map[string]any{"baseURL": "http://gate:8080/t/" + c.token, "apiKey": "hint-proxy"},
			"models":  map[string]any{model: map[string]any{"name": model}}}},
		"autoupdate": false, "share": "disabled"})
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	epoch := time.Unix(0, 0)
	for _, h := range []*tar.Header{
		{Name: "opt/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "opt/hint/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "opt/hint/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "opt/hint/bin/opencode", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bin))},
		{Name: "tmp/.config/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65534, Gid: 65534},
		{Name: "tmp/.config/opencode/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65534, Gid: 65534},
		{Name: "tmp/.config/opencode/opencode.json", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 65534, Gid: 65534, Size: int64(len(cfg))},
	} {
		h.ModTime, h.Format = epoch, tar.FormatPAX
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		switch h.Name {
		case "opt/hint/bin/opencode":
			tw.Write(bin)
		case "tmp/.config/opencode/opencode.json":
			tw.Write(cfg)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// openCodeArgv is the fixed command of one attempt (no shell).
func openCodeArgv(model, instruction string) []string {
	return []string{"/opt/hint/bin/opencode", "run", "--standalone", "--format", "json", "--thinking", "--auto", "-m", "hint/" + model, "--", instruction}
}

// taskInstruction is the instruction of an exported task without its leading canary comment.
func taskInstruction(taskDir string) (string, error) {
	b, err := readFileBounded(filepath.Join(taskDir, "instruction.md"), 1<<20)
	if err != nil {
		return "", err
	}
	s := string(b)
	if strings.HasPrefix(s, "<!-- canary ") {
		if i := strings.Index(s, "-->\n"); i >= 0 {
			s = s[i+4:]
		}
	}
	return s, nil
}

func readFileBounded(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBounded(f, max)
}

// openCodeExecutionDigest is the per-attempt execution provenance (B, local only): the argv
// with the instruction replaced by its digest, the config without the attempt's token, the
// version, the target and the installed binary's digest.
func openCodeExecutionDigest(model, instruction string, prov map[string]any) string {
	is := sha256.Sum256([]byte(instruction))
	argv := openCodeArgv(model, "sha256:"+hex.EncodeToString(is[:]))
	cfg, _ := openCodeFiles(nil, &Counter{token: "<token>"}, model)
	cs := sha256.Sum256(cfg)
	b, _ := json.Marshal([]any{argv, hex.EncodeToString(cs[:]), prov["version"], prov["binary_sha256"], prov["target"]})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DockerLaunchDigest is the canonical configuration identity (A) of a controlled OpenCode run
// in the container runner: the same for the work-unit manifest and every attempt, independent
// of the task, its instruction, the attempt's token, ports and proxy host. It names the runner
// version, the gate policy, the agent, the pinned version, the selected target and its exact
// archive pin, the normalized argv and config, the environment and the container controls. An
// unpinned target has no configuration.
func DockerLaunchDigest(version, model, target string) (string, error) {
	pin := OpenCodePins[version][target]
	if pin == "" {
		return "", fmt.Errorf("OpenCode %s has no pin for target %q", version, target)
	}
	cfg, err := openCodeFiles(nil, &Counter{token: "<token>"}, model)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(bytes.NewReader(cfg))
	var conf []byte
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "tmp/.config/opencode/opencode.json" {
			conf, _ = io.ReadAll(tr)
		}
	}
	policy := sha256.Sum256([]byte(gatePolicyText))
	b, _ := json.Marshal([]any{"hint-docker-launch-v1", DockerRunnerVersion, GatePolicy, hex.EncodeToString(policy[:]),
		"opencode", version, target, pin, openCodeArgv(model, "<instruction>"), string(conf),
		[]string{"HOME=/tmp", "OPENAI_API_KEY=hint-proxy", "OPENAI_BASE_URL=http://gate:8080/t/<token>"},
		map[string]any{"user": "65534:65534", "cap_drop": "ALL", "no_new_privileges": true, "tree_owner": "65534:65534", "binary": "root:0755"}})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PreflightTarget builds a task's agent image and resolves its OpenCode target with the clean
// probe, before any run is planned (the target is part of the configuration).
func (d *Docker) PreflightTarget(ctx context.Context, taskDir string) (string, error) {
	id, _, _, err := d.build(ctx, filepath.Join(taskDir, "environment"), "agent")
	if err != nil {
		return "", err
	}
	t, _, err := d.openCodeTarget(ctx, id)
	return t, err
}
