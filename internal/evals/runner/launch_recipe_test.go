package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestOrdinaryLaunchFrozenVectors(t *testing.T) {
	files, _ := openCodeFiles([]byte("BIN"), &Counter{token: "tok"}, "m")
	h := sha256.Sum256(files)
	if hex.EncodeToString(h[:]) != "1d95c598524057408dc83c118930ed475615aac0e128a5f23f938b2ce76c0201" {
		t.Fatal("ordinary install bytes changed")
	}
	if b := openCodeExecutionDigest("m", "do", map[string]any{"version": "2.0.26", "binary_sha256": "sha256:a", "target": "linux-arm64"}); b != "sha256:cccb521b296f1b37600404929ddc352d3be1e4bdbbd649251366389c37c4c920" {
		t.Fatal("ordinary B changed", b)
	}
	for target, want := range map[string]string{"linux-arm64": "63a12350aedff888770f7e970b7228d7a2c8372cc305f6273c18698e9875e934", "linux-arm64-musl": "e27972668b4eb49c4d0a4ea794e78424c8f5c5754775785ee6f891d2c5906eb6", "linux-x64": "6d6033df8bddfb9fe22b65008a22235707e847ae17290cc7e69bc15e7aa6c4f4", "linux-x64-musl": "cdd6721a045afafffa80259e1f5d3b88582d699219b5b9e13c9723c3290906d2"} {
		got, err := DockerLaunchDigestWithPolicy("2.0.26", "m", target, "")
		if err != nil || got != "sha256:"+want {
			t.Fatal(target, got, err)
		}
	}
	if !reflect.DeepEqual(openCodeArgv("m", "do"), []string{"/opt/hint/bin/opencode", "run", "--standalone", "--format", "json", "--thinking", "--auto", "--title", "hint-trial", "-m", "hint/m", "--", "do"}) || !reflect.DeepEqual(gateEnv(&Counter{token: "tok"}), []string{"OPENAI_BASE_URL=http://gate:8080/t/tok", "OPENAI_API_KEY=hint-proxy", "HOME=/tmp"}) {
		t.Fatal("ordinary argv/environment changed")
	}
}
func TestStudyRecipeRendersControlsAndIdentities(t *testing.T) {
	r, err := makeOpenCodeRecipe("m", "do; $(x)", "secret-attempt-token", StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Argv[:2], []string{studyLauncher, "-i"}) || !reflect.DeepEqual(r.Argv[2:2+len(r.Env)], r.Env) || r.Argv[len(r.Argv)-1] != "do; $(x)" {
		t.Fatal("argv does not render recipe environment")
	}
	home := false
	for _, file := range r.Files {
		if file.Name == "opt/hint/study/agent-home/" && file.UID == 0 && file.Mode == 0555 {
			home = true
		}
		if file.Name == "opt/hint/study/home/" {
			t.Fatal("ambiguous home directory remains in inventory")
		}
	}
	if !home {
		t.Fatal("missing dedicated agent-home inventory")
	}
	matched := false
	for _, item := range r.Env {
		matched = matched || item == "HOME="+studyRoot+"/agent-home"
	}
	if !matched {
		t.Fatal("HOME does not select agent-home")
	}
	var config map[string]any
	json.Unmarshal(r.Config, &config)
	if config["permission"].(map[string]any)["task"] != "deny" || config["permissions"].([]any)[0].(map[string]any)["action"] != "subagent" || config["experimental"].(map[string]any)["subagent_depth"] != float64(0) {
		t.Fatal("native policy missing")
	}
	archive, _ := r.archive([]byte("BIN"))
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
		if h.Name == studyConfig {
			body, _ := io.ReadAll(tr)
			if !bytes.Equal(body, r.Config) || h.Uid != 0 || h.Mode != 0444 {
				t.Fatal("actual config differs from recipe")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing actual config")
	}
	ordinary, _ := DockerLaunchDigest("2.0.26", "m", "linux-arm64")
	a, err := DockerLaunchDigestWithPolicy("2.0.26", "m", "linux-arm64", StudyLaunchSingleRootV1)
	if err != nil || a == ordinary {
		t.Fatal("study A not distinct", err)
	}
	again, _ := DockerLaunchDigestWithPolicy("2.0.26", "m", "linux-arm64", StudyLaunchSingleRootV1)
	if a != again {
		t.Fatal("study A unstable")
	}
	p := map[string]any{"version": "2.0.26", "target": "linux-arm64", "binary_sha256": "sha256:" + strings.Repeat("b", 64)}
	b, err := openCodeExecutionDigestWithPolicy("m", "do", p, fakeGateID, StudyLaunchSingleRootV1)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"do other", "do"} {
		image := fakeGateID
		if other == "do" {
			image = "sha256:" + strings.Repeat("c", 64)
		}
		changed, _ := openCodeExecutionDigestWithPolicy("m", other, p, image, StudyLaunchSingleRootV1)
		if changed == b {
			t.Fatal("B omitted instruction/image identity")
		}
	}
	normalized, _ := makeOpenCodeRecipe("m", "<instruction>", "<token>", StudyLaunchSingleRootV1)
	raw, _ := json.Marshal(normalized.identity())
	if bytes.Contains(raw, []byte("secret-attempt-token")) {
		t.Fatal("attempt token in identity")
	}
}
func TestStudyOptionsRefuseBeforeEngineAccess(t *testing.T) {
	d := &Docker{}
	for _, o := range []RunOptions{{Agent: "opencode", StudyLaunchPolicy: "unknown"}, {Agent: "opencode", StudyLaunchPolicy: StudyLaunchSingleRootV1}, {Agent: "opencode", StudyLaunchPolicy: StudyLaunchSingleRootV1, CapturePrelaunchTree: true}, {Agent: "oracle", StudyLaunchPolicy: StudyLaunchSingleRootV1, CapturePrelaunchTree: true, ExpectLaunchDigest: fakeGateID}} {
		if _, err := d.Run(context.Background(), "unused", o); err == nil {
			t.Fatal("invalid policy reached engine")
		}
	}
	if _, err := DockerLaunchDigestWithPolicy("2.0.26", "m", "linux-arm64", "unknown"); err == nil {
		t.Fatal("unknown policy accepted")
	}
	if _, err := DockerLaunchDigestWithPolicy("2.0.22", "m", "linux-arm64", StudyLaunchSingleRootV1); err == nil {
		t.Fatal("unreviewed version accepted")
	}
}
