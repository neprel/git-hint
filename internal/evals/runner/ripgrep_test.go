package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestRipgrepBoundedOfflineAdmission(t *testing.T) {
	b := []byte("synthetic executable-shaped bytes; never executed")
	h := sha256.Sum256(b)
	digest := "sha256:" + hex.EncodeToString(h[:])
	p := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if v, e := readRipgrepBinary(context.Background(), p, int64(len(b)), digest); e == nil || v != nil {
			t.Fatal("unsupported host admitted")
		}
		return
	}
	got, err := readRipgrepBinary(context.Background(), p, int64(len(b)), digest)
	if err != nil || !bytes.Equal(got, b) {
		t.Fatal("bounded synthetic positive", err)
	}
	for _, tc := range []struct {
		name   string
		size   int64
		digest string
	}{{"short", int64(len(b) - 1), digest}, {"long", int64(len(b) + 1), digest}, {"wronghash", int64(len(b)), "sha256:" + strings.Repeat("0", 64)}, {"emptycap", 0, digest}, {"overcap", ripgrepSize + 1, digest}} {
		t.Run(tc.name, func(t *testing.T) {
			if v, e := readRipgrepBinary(context.Background(), p, tc.size, tc.digest); e == nil || v != nil {
				t.Fatal("unverified artifact admitted")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, e := readRipgrepBinary(ctx, p, int64(len(b)), digest); e == nil || v != nil {
		t.Fatal("cancel admitted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Dir(p), filepath.Join(t.TempDir(), "missing"), ""} {
		if v, e := readRipgrepBinary(context.Background(), path, int64(len(b)), digest); e == nil || v != nil {
			t.Fatal("unsafe path admitted")
		}
	}
	if _, e := (RipgrepSource{BinaryFile: p}).binary(context.Background(), "linux-arm64"); e == nil {
		t.Fatal("synthetic bytes matched production pin")
	}
	for _, target := range []string{"linux-arm64-musl", "linux-x64", "darwin-arm64", ""} {
		if _, e := (RipgrepSource{BinaryFile: p}).binary(context.Background(), target); e == nil {
			t.Fatal("unsupported target admitted")
		}
	}
}

func TestRipgrepRecipePayloadAndIdentity(t *testing.T) {
	old, err := makeOpenCodeRecipe("m", "do", "secret-token", StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := old.withRipgrep("linux-arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old.Argv, r.Argv) || !reflect.DeepEqual(old.Env, r.Env) || !bytes.Equal(old.Config, r.Config) || len(r.Files) != len(old.Files)+1 {
		t.Fatal("permissions, argv, env or config changed")
	}
	archive, err := r.archiveWithRipgrep([]byte("opencode"), []byte("synthetic-rg"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	found := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == ripgrepInstall {
			found = true
			if h.Typeflag != tar.TypeReg || h.Mode != 0755 || h.Uid != 0 || h.Gid != 0 || string(body) != "synthetic-rg" {
				t.Fatal("wrong installed payload")
			}
		}
	}
	if !found {
		t.Fatal("rg missing")
	}
	if _, e := r.archive([]byte("opencode")); e == nil {
		t.Fatal("missing required payload accepted")
	}
	legacy, err := DockerLaunchDigestWithPolicy(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := DockerLaunchDigestWithRipgrep(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil || planned == legacy {
		t.Fatal("runtime identity not distinct", err)
	}
	same, _ := dockerLaunchDigestRuntime(DockerOpenCodeVersion, "m", "linux-arm64", StudyLaunchSingleRootV1, true)
	if planned != same {
		t.Fatal("planned and actual use different recipes")
	}
	prov := map[string]any{"version": DockerOpenCodeVersion, "target": "linux-arm64", "binary_sha256": "sha256:" + strings.Repeat("b", 64), "ripgrep_sha256": ripgrepSHA256}
	b, e := openCodeExecutionDigestRuntime("m", "do", prov, fakeGateID, StudyLaunchSingleRootV1, true)
	if e != nil {
		t.Fatal(e)
	}
	oldB, _ := openCodeExecutionDigestWithPolicy("m", "do", prov, fakeGateID, StudyLaunchSingleRootV1)
	if b == oldB {
		t.Fatal("B ignores rg")
	}
	changed, _ := openCodeExecutionDigestRuntime("m", "changed", prov, fakeGateID, StudyLaunchSingleRootV1, true)
	if b == changed {
		t.Fatal("B ignores instruction")
	}
	prov["ripgrep_sha256"] = "sha256:" + strings.Repeat("0", 64)
	if _, e := openCodeExecutionDigestRuntime("m", "do", prov, fakeGateID, StudyLaunchSingleRootV1, true); e == nil {
		t.Fatal("unverified rg B admitted")
	}
	for _, target := range []string{"linux-arm64-musl", "linux-x64", ""} {
		if _, e := DockerLaunchDigestWithRipgrep(DockerOpenCodeVersion, "m", target, StudyLaunchSingleRootV1); e == nil {
			t.Fatal("unsupported runtime target")
		}
	}
	if _, e := DockerLaunchDigestWithRipgrep(DockerOpenCodeVersion, "m", "linux-arm64", ""); e == nil {
		t.Fatal("ordinary runtime enabled")
	}
	if _, e := DockerLaunchDigestWithRipgrep("2.0.22", "m", "linux-arm64", StudyLaunchSingleRootV1); e == nil {
		t.Fatal("unreviewed OpenCode admitted")
	}
}

func TestRipgrepOptInRequiresStudy(t *testing.T) {
	good := RunOptions{Agent: "opencode", StudyLaunchPolicy: StudyLaunchSingleRootV1, CapturePrelaunchTree: true, ExpectLaunchDigest: "sha256:" + strings.Repeat("a", 64), OpenCode: &OpenCodeSource{Version: DockerOpenCodeVersion}, Ripgrep: &RipgrepSource{BinaryFile: "local-rg"}}
	if e := good.checkStudyLaunch(); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"policy", "agent", "capture", "planned", "source", "version", "path"} {
		t.Run(name, func(t *testing.T) {
			v := good
			src := *good.OpenCode
			v.OpenCode = &src
			rg := *good.Ripgrep
			v.Ripgrep = &rg
			switch name {
			case "policy":
				v.StudyLaunchPolicy = ""
			case "agent":
				v.Agent = "oracle"
			case "capture":
				v.CapturePrelaunchTree = false
			case "planned":
				v.ExpectLaunchDigest = ""
			case "source":
				v.OpenCode = nil
			case "version":
				v.OpenCode.Version = "2.0.22"
			case "path":
				v.Ripgrep.BinaryFile = ""
			}
			if e := v.checkStudyLaunch(); e == nil {
				t.Fatal("invalid opt-in admitted")
			}
		})
	}
}

func TestRipgrepFakeEngineInvalidArtifactHasZeroStarts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(p, []byte("synthetic, wrong pin"), 0600); err != nil {
		t.Fatal(err)
	}
	r, c, s := studyFake(t, studyFakeOptions{ripgrepFile: p})
	if r.ExceptionInfo == nil || s.starts != 0 || c.Report().ComponentsN["prompt"] != 0 {
		t.Fatal("invalid artifact reached agent")
	}
}

func TestRipgrepReviewedIdentityLiteral(t *testing.T) {
	got, e := ripgrepIdentity("linux-arm64")
	if e != nil {
		t.Fatal(e)
	}
	want := []any{"opencode-offline-ripgrep-v1", "15.1.0", "linux-arm64", "sha256:968cabe8efed72fd8fd482cb76b6084fcb695fc5293af7fb62296b02f487fb69", int64(4543848), "/opt/hint/bin/rg", "root:root:0755", "gnu-aarch64-glibc-2.17-2.18", "required_absent_before_install", "same_container_exact_file_after_install", int64(4609384), "strict_complete_tar_v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("reviewed dependency identity changed")
	}
}
