package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// C5 (07B-I, 07B-J §21): the resolved runtime provenance of a job, written by the runner.
// Built images are identified by the engine's image ID and every container is created by
// that ID (a cache tag or a context digest is only a hint). The pinned base reference of a
// Dockerfile stays apart from the built image: the built image's ID or RepoDigests are never
// the base identity. The toolchain is observed in a clean probe of the base image (no
// candidate code and no project setup run there). Lock digests describe manifests only, not
// every installed dependency. Nothing here enters the eval report.

// image is a built or pinned image as the engine resolved it.
type image struct {
	ID          string   `json:"id"`
	RepoDigests []string `json:"repo_digests,omitempty"`
	OS          string   `json:"os,omitempty"`
	Arch        string   `json:"architecture,omitempty"`
	Variant     string   `json:"variant,omitempty"`
}

// resolveImage returns the engine's identity of an image reference or ID.
func (d *Docker) resolveImage(ctx context.Context, ref string) (image, error) {
	r, err := d.cli.ImageInspect(ctx, ref)
	if err != nil {
		return image{}, err
	}
	if !strings.HasPrefix(r.ID, "sha256:") {
		return image{}, fmt.Errorf("the engine gave no image ID for %s", ref)
	}
	return image{ID: r.ID, RepoDigests: r.RepoDigests, OS: r.Os, Arch: r.Architecture, Variant: r.Variant}, nil
}

// create creates a container and checks that the engine created it from exactly the
// requested image ID.
func (d *Docker) create(ctx context.Context, cfg *container.Config, host *container.HostConfig) (string, error) {
	if !strings.HasPrefix(cfg.Image, "sha256:") {
		return "", fmt.Errorf("a container must be created by image ID, not %q", cfg.Image)
	}
	c, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: host})
	if err != nil {
		return "", err
	}
	ins, err := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil || ins.Container.Image != cfg.Image {
		d.remove(c.ID)
		return "", fmt.Errorf("the container does not use the recorded image %s", cfg.Image)
	}
	return c.ID, nil
}

// pinnedFrom returns the FROM references of a Dockerfile; each must name a digest.
func pinnedFrom(dockerfile string) ([]string, error) {
	b, err := os.ReadFile(dockerfile)
	if err != nil {
		return nil, err
	}
	var refs []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		ref := ""
		for _, x := range f[1:] {
			if !strings.HasPrefix(x, "--") {
				ref = x
				break
			}
		}
		if ref == "scratch" {
			continue
		}
		if !strings.Contains(ref, "@sha256:") {
			return nil, fmt.Errorf("the base image %q is not pinned by digest", ref)
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, errors.New("no FROM in the Dockerfile")
	}
	return refs, nil
}

// lockFiles are the dependency manifests whose digests are recorded.
var lockFiles = []string{"go.mod", "go.sum", "requirements.txt", "requirements-dev.txt", "poetry.lock", "pyproject.toml", "Pipfile.lock"}

// toolchainArgv is the fixed probe command of a task's language (no shell).
func toolchainArgv(repo string) []string {
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err == nil {
		return []string{"go", "version"}
	}
	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt", "setup.cfg"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			return []string{"python", "--version"}
		}
	}
	return nil
}

// probeToolchain runs the fixed argv in a fresh container of the base image: unprivileged,
// no network, no capabilities, finite time and output. Any failure is unknown.
func (d *Docker) probeToolchain(ctx context.Context, baseID string, argv []string) map[string]any {
	out := map[string]any{"argv": argv, "image": baseID, "user": "65534:65534", "network": "none", "state": "unknown"}
	if argv == nil {
		out["reason"] = "no_known_language"
		return out
	}
	pids := int64(64)
	id, err := d.create(ctx, &container.Config{Image: baseID, Cmd: argv, User: "65534:65534", WorkingDir: "/"},
		&container.HostConfig{NetworkMode: "none", CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			Resources: container.Resources{Memory: 256 << 20, PidsLimit: &pids}})
	if err != nil {
		out["reason"] = "probe_not_created"
		return out
	}
	defer d.remove(id)
	runErr := d.runToEnd(ctx, id, 30*time.Second, "toolchain probe")
	if runErr != nil {
		out["reason"] = "probe_failed"
		return out
	}
	// the output counts only when captured completely within one 4 KiB budget (C7BJ-11)
	logs, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		out["reason"] = "output_unavailable"
		return out
	}
	b := &capWriter{max: 4 << 10}
	derr := demux(b, b, logs)
	logs.Close()
	switch {
	case derr != nil:
		out["reason"] = "output_incomplete"
		return out
	case b.over:
		out["reason"] = "output_truncated"
		return out
	case strings.TrimSpace(b.buf.String()) == "":
		out["reason"] = "output_empty"
		return out
	}
	out["output"] = strings.TrimSpace(b.buf.String())
	out["state"] = "observed"
	return out
}

// jobProvenance resolves the provenance of one job.
func (d *Docker) jobProvenance(ctx context.Context, taskDir string, im images) (map[string]any, error) {
	p := map[string]any{"runner": DockerRunnerVersion, "agent_binary": "unknown"}
	if info, err := d.cli.Info(ctx, client.InfoOptions{}); err == nil {
		in := info.Info
		p["engine"] = map[string]any{"id": in.ID, "version": in.ServerVersion, "os_type": in.OSType, "operating_system": in.OperatingSystem,
			"kernel": in.KernelVersion, "architecture": in.Architecture}
	} else {
		p["engine"] = map[string]any{"state": "unknown"}
	}
	bases := map[string]any{}
	var toolchainBase string
	for _, kind := range []string{"environment", "tests"} {
		if kind == "tests" && im.verifier == "" {
			continue
		}
		refs, err := pinnedFrom(filepath.Join(taskDir, kind, "Dockerfile"))
		if err != nil {
			return nil, err
		}
		var list []any
		for _, ref := range refs {
			b := map[string]any{"pinned": ref}
			if r, err := d.resolveImage(ctx, ref); err == nil {
				b["resolved"] = r
				if kind == "environment" && toolchainBase == "" {
					toolchainBase = r.ID
				}
			} else {
				b["resolved"] = "unknown"
			}
			list = append(list, b)
		}
		bases[kind] = list
	}
	p["base_images"] = bases
	built := map[string]any{"agent": im.agentImage, "agent_context": im.agentCtx, "agent_cache_tag": im.agentTag}
	if im.verifier != "" {
		built["verifier"], built["verifier_context"], built["verifier_cache_tag"] = im.verifierImage, im.verifierCtx, im.verifierTag
	}
	p["built_images"] = built
	repo := filepath.Join(taskDir, "environment", "repo")
	if toolchainBase != "" {
		p["toolchain"] = d.probeToolchain(ctx, toolchainBase, toolchainArgv(repo))
	} else {
		p["toolchain"] = map[string]any{"state": "unknown", "reason": "base_not_resolved"}
	}
	locks := map[string]string{}
	for _, f := range lockFiles {
		if b, err := os.ReadFile(filepath.Join(repo, f)); err == nil {
			sum := sha256.Sum256(b)
			locks[f] = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	p["dependency_manifests"] = map[string]any{"scope": "manifests_only", "files": sortedMap(locks)}
	return p, nil
}

func sortedMap(m map[string]string) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []any{}
	for _, k := range keys {
		out = append(out, map[string]any{"path": k, "digest": m[k]})
	}
	return out
}
