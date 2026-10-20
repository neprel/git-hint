package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func TestPinnedFrom(t *testing.T) {
	dir := t.TempDir()
	w := func(s string) string {
		p := filepath.Join(dir, "Dockerfile")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	refs, err := pinnedFrom(w("FROM --platform=linux/arm64 " + baseImage + " AS b\nRUN true\nFROM scratch\nCOPY --from=b /x /x\n"))
	if err != nil || len(refs) != 1 || refs[0] != baseImage {
		t.Fatalf("%v %v", refs, err)
	}
	for _, bad := range []string{"FROM ubuntu:24.04\n", "FROM ubuntu\n", "RUN true\n", "FROM scratch\n"} {
		if _, err := pinnedFrom(w(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	repo := t.TempDir()
	if toolchainArgv(repo) != nil {
		t.Fatal("a language without manifests")
	}
	os.WriteFile(filepath.Join(repo, "requirements.txt"), nil, 0o644)
	if strings.Join(toolchainArgv(repo), " ") != "python --version" {
		t.Fatal("python")
	}
	os.WriteFile(filepath.Join(repo, "go.mod"), nil, 0o644)
	if strings.Join(toolchainArgv(repo), " ") != "go version" {
		t.Fatal("go")
	}
}

// A container is never created from a tag or name, only from an engine image ID.
func TestCreateNeedsAnImageID(t *testing.T) {
	d := &Docker{}
	for _, ref := range []string{"ubuntu:24.04", baseImage, "hint-eval-agent:0123456789abcdef0123", ""} {
		if _, err := d.create(context.Background(), &container.Config{Image: ref}, &container.HostConfig{}); err == nil {
			t.Errorf("%q accepted", ref)
		}
	}
}

// C5 on the engine: containers run from the image ID the build reported; a moved cache tag
// changes nothing; the provenance separates the pinned base from the built image, observes
// the toolchain in the base (unknown when the base has none), and lists the manifests.
func TestDockerRunnerProvenance(t *testing.T) {
	d := dockerOrSkip(t)
	defer d.Close()
	task := syntheticTask(t, solveFixed, 120)
	os.WriteFile(filepath.Join(task, "environment", "repo", "go.mod"), []byte("module x\n\ngo 1.26\n"), 0o644)
	ctx := context.Background()
	jobs := t.TempDir()
	read := func(job string) trialResult {
		var r trialResult
		b, _ := os.ReadFile(filepath.Join(jobs, job, job+"__1", "result.json"))
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err := d.Run(ctx, task, RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "a"}); err != nil {
		t.Fatal(err)
	}
	first := read("a")
	id, _ := first.Container["agent_image"].(string)
	if !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("agent image %v", first.Container["agent_image"])
	}
	p := first.Container["provenance"].(map[string]any)
	bases := p["base_images"].(map[string]any)["environment"].([]any)[0].(map[string]any)
	resolved, _ := bases["resolved"].(map[string]any)
	if bases["pinned"] != baseImage || resolved == nil || !strings.HasPrefix(resolved["id"].(string), "sha256:") || resolved["id"] == id {
		t.Fatalf("base %v (built %s)", bases, id)
	}
	built := p["built_images"].(map[string]any)
	if built["agent"].(map[string]any)["id"] != id || !strings.HasPrefix(built["agent_cache_tag"].(string), "hint-eval-agent:") {
		t.Fatalf("built %v", built)
	}
	tc := p["toolchain"].(map[string]any)
	if tc["state"] != "unknown" || tc["reason"] != "probe_failed" || strings.Join(toStrings(tc["argv"]), " ") != "go version" || tc["image"] != resolved["id"] {
		t.Fatalf("toolchain in a base without Go: %v", tc) // ubuntu has no go: observed as unknown, never invented
	}
	dm := p["dependency_manifests"].(map[string]any)
	if dm["scope"] != "manifests_only" || len(dm["files"].([]any)) != 1 || p["agent_binary"] != "unknown" {
		t.Fatalf("manifests %v", p)
	}
	if eng, _ := p["engine"].(map[string]any); eng["version"] == "" || eng["architecture"] == "" {
		t.Fatalf("engine %v", p["engine"])
	}
	// move the cache tag to another image: the next run still uses the image of the context
	tag := built["agent_cache_tag"].(string)
	if _, err := d.cli.ImageTag(ctx, client.ImageTagOptions{Source: baseImage, Target: tag}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(ctx, task, RunOptions{Agent: "oracle", JobsDir: jobs, JobName: "b"}); err != nil {
		t.Fatal(err)
	}
	if second := read("b"); second.Container["agent_image"] != id {
		t.Fatalf("a moved tag changed the image: %v vs %s", second.Container["agent_image"], id)
	}
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
