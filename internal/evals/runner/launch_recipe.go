package runner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// StudyLaunchPolicy is an internal controlled-study opt-in, never a CLI/default setting.
type StudyLaunchPolicy string

const StudyLaunchSingleRootV1 StudyLaunchPolicy = "single-root-v1"

// Check validates only the policy name; it grants no eligibility.
func (p StudyLaunchPolicy) Check() error {
	if p != "" && p != StudyLaunchSingleRootV1 {
		return errors.New("unsupported study launch policy")
	}
	return nil
}

// SupportsStudyLaunchPolicy describes the runner implementation, not a prepared run.
func (d *Docker) SupportsStudyLaunchPolicy(p StudyLaunchPolicy) bool { return p.Check() == nil }
func (o RunOptions) checkStudyLaunch() error {
	if o.ExpectedBackend != nil {
		t, ref := o.captureOptions()
		if !completeExpectedBackend(o.ExpectedBackend) || t == nil || ref == nil {
			return errors.New("expected backend requires complete metadata and capture transport")
		}
	}
	if o.Ripgrep != nil && (o.StudyLaunchPolicy != StudyLaunchSingleRootV1 || o.Ripgrep.BinaryFile == "" || o.OpenCode == nil || o.OpenCode.Version != DockerOpenCodeVersion) {
		return errors.New("offline ripgrep requires the pinned OpenCode study runtime")
	}
	if err := o.StudyLaunchPolicy.Check(); err != nil {
		return err
	}
	if o.StudyLaunchPolicy != "" && (o.Agent != "opencode" || !o.CapturePrelaunchTree) {
		return errors.New("study launch requires opencode and prelaunch tree capture")
	}
	if o.StudyLaunchPolicy != "" && !prelaunchDigestPattern.MatchString(o.ExpectLaunchDigest) {
		return errors.New("study launch needs a frozen planned launch digest")
	}
	if o.BeforeAgentStart != nil && o.StudyLaunchPolicy == "" {
		return errors.New("a launch hook requires a study launch")
	}
	return nil
}

// LaunchFacts are the actual launch facts of an attempt after every prestart check passed: the
// actual launch digest A (equal to the frozen plan), the execution digest the agent run records,
// the gate policy identity that entered A, the installed target and the model.
type LaunchFacts struct {
	LaunchDigest, ExecutionDigest, GatePolicyDigest, Target, Model string
}

// GatePolicyDigest is the identity of the network gate policy: the same GatePolicy version and
// policy text digest that enter the launch digest A.
func GatePolicyDigest() string {
	gate := sha256.Sum256([]byte(gatePolicyText))
	return launchHash([]any{"hint-gate-policy-v1", GatePolicy, hex.EncodeToString(gate[:])})
}

type launchFile struct {
	Name     string
	Type     byte
	Mode     int64
	UID, GID int
	Body     string // binary is supplied only when serializing actual install bytes
	Binary   bool
}
type openCodeRecipe struct {
	Ripgrep   bool
	Policy    StudyLaunchPolicy
	Argv, Env []string
	Config    []byte
	Files     []launchFile
}

const studyRoot = "/opt/hint/study"
const studyConfig = "opt/hint/study/config/opencode.json"
const studyLauncher = "/usr/bin/env"

func makeOpenCodeRecipe(model, instruction, token string, p StudyLaunchPolicy) (openCodeRecipe, error) {
	var r openCodeRecipe
	if err := p.Check(); err != nil {
		return r, err
	}
	if !openCodeModel.MatchString(model) {
		return r, errors.New("unexpected model name")
	}
	r.Policy = p
	config := map[string]any{
		"provider": map[string]any{"hint": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "hint",
			"options": map[string]any{"baseURL": "http://gate:8080/t/" + token, "apiKey": "hint-proxy"},
			"models":  map[string]any{model: map[string]any{"name": model}}}},
		"autoupdate": false, "share": "disabled",
	}
	r.Argv = baseOpenCodeArgv(model, instruction)
	r.Env = openCodeEnvironment(token, p)
	r.Files = []launchFile{{Name: "opt/", Type: tar.TypeDir, Mode: 0755}, {Name: "opt/hint/", Type: tar.TypeDir, Mode: 0755}, {Name: "opt/hint/bin/", Type: tar.TypeDir, Mode: 0755}, {Name: "opt/hint/bin/opencode", Type: tar.TypeReg, Mode: 0755, Binary: true}}
	if p == "" {
		r.Files = append(r.Files, launchFile{Name: "tmp/.config/", Type: tar.TypeDir, Mode: 0700, UID: 65534, GID: 65534}, launchFile{Name: "tmp/.config/opencode/", Type: tar.TypeDir, Mode: 0700, UID: 65534, GID: 65534})
	} else {
		config["permission"] = map[string]string{"task": "deny"}
		config["permissions"] = []map[string]string{{"action": "subagent", "resource": "*", "effect": "deny"}}
		config["experimental"] = map[string]int{"subagent_depth": 0}
		for _, name := range []string{"", "config/", "agent-home/", "xdg-config/", "runtime/"} {
			r.Files = append(r.Files, launchFile{Name: "opt/hint/study/" + name, Type: tar.TypeDir, Mode: 0555})
		}
		for _, name := range []string{"data/", "state/", "cache/", "tmp/"} {
			r.Files = append(r.Files, launchFile{Name: "opt/hint/study/runtime/" + name, Type: tar.TypeDir, Mode: 0700, UID: 65534, GID: 65534})
		}
		// No shell. The explicit agent and fixed title avoid environment/config selection.
		args := append([]string{}, r.Argv...)
		args = append(args[:2], append([]string{"--agent", "build"}, args[2:]...)...)
		r.Argv = append([]string{studyLauncher, "-i"}, r.Env...)
		r.Argv = append(r.Argv, args...)
	}
	r.Config, _ = json.Marshal(config)
	if p == "" {
		r.Files = append(r.Files, launchFile{Name: "tmp/.config/opencode/opencode.json", Type: tar.TypeReg, Mode: 0600, UID: 65534, GID: 65534, Body: string(r.Config)})
	} else {
		r.Files = append(r.Files, launchFile{Name: studyConfig, Type: tar.TypeReg, Mode: 0444, Body: string(r.Config)})
	}
	return r, nil
}

func (r openCodeRecipe) withRipgrep(target string) (openCodeRecipe, error) {
	if r.Policy != StudyLaunchSingleRootV1 {
		return r, errors.New("offline ripgrep requires a study recipe")
	}
	if _, err := ripgrepIdentity(target); err != nil {
		return r, err
	}
	r.Ripgrep = true
	r.Files = append(r.Files, launchFile{Name: ripgrepInstall, Type: tar.TypeReg, Mode: 0755, Binary: true})
	return r, nil
}

func openCodeEnvironment(token string, p StudyLaunchPolicy) []string {
	if p == "" {
		return []string{"OPENAI_BASE_URL=http://gate:8080/t/" + token, "OPENAI_API_KEY=hint-proxy", "HOME=/tmp"}
	}
	return []string{"HOME=" + studyRoot + "/agent-home", "XDG_CONFIG_HOME=" + studyRoot + "/xdg-config", "XDG_DATA_HOME=" + studyRoot + "/runtime/data", "XDG_STATE_HOME=" + studyRoot + "/runtime/state", "XDG_CACHE_HOME=" + studyRoot + "/runtime/cache", "TMPDIR=" + studyRoot + "/runtime/tmp", "PATH=/opt/hint/bin:/usr/local/bin:/usr/bin:/bin", "OPENCODE_CONFIG_DIR=" + studyRoot + "/config", "OPENCODE_CONFIG_PROJECT_DISABLE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENAI_BASE_URL=http://gate:8080/t/" + token, "OPENAI_API_KEY=hint-proxy"}
}
func (r openCodeRecipe) archive(binary []byte) ([]byte, error) {
	return r.archiveWithRipgrep(binary, nil)
}
func (r openCodeRecipe) archiveWithRipgrep(binary, rg []byte) ([]byte, error) {
	if r.Ripgrep && len(rg) == 0 {
		return nil, errors.New("offline ripgrep payload is missing")
	}
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, file := range r.Files {
		body := []byte(file.Body)
		if file.Binary {
			body = binary
			if file.Name == ripgrepInstall {
				body = rg
			}
		}
		h := &tar.Header{Name: file.Name, Typeflag: file.Type, Mode: file.Mode, Uid: file.UID, Gid: file.GID, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if file.Type == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := w.WriteHeader(h); err != nil {
			return nil, err
		}
		if file.Type == tar.TypeReg {
			if _, err := w.Write(body); err != nil {
				return nil, err
			}
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
func launchHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}
func (r openCodeRecipe) identity() any {
	return []any{r.Policy, r.Argv, r.Env, string(r.Config), r.Files, "empty_state_required", "prelaunch_capture_required", "inspection_only_requires_real_binary_validation", studyInspectionRules()}
}

// DockerLaunchDigestWithPolicy renders the same recipe used by the actual runner.
// Empty policy retains the exact legacy identity. Nonempty policy is not eligibility proof.
func DockerLaunchDigestWithPolicy(version, model, target string, p StudyLaunchPolicy) (string, error) {
	return dockerLaunchDigestRuntime(version, model, target, p, false)
}

// DockerLaunchDigestWithRipgrep selects the same study controls with one required,
// pinned offline GNU aarch64 ripgrep payload. It does not validate an image ABI.
func DockerLaunchDigestWithRipgrep(version, model, target string, p StudyLaunchPolicy) (string, error) {
	return dockerLaunchDigestRuntime(version, model, target, p, true)
}
func dockerLaunchDigestRuntime(version, model, target string, p StudyLaunchPolicy, rg bool) (string, error) {
	if p != "" && version != DockerOpenCodeVersion {
		return "", errors.New("study launch requires reviewed OpenCode version")
	}
	pin := OpenCodePins[version][target]
	if pin == "" {
		return "", fmt.Errorf("OpenCode %s has no pin for target %q", version, target)
	}
	r, err := makeOpenCodeRecipe(model, "<instruction>", "<token>", p)
	if err != nil {
		return "", err
	}
	if rg {
		r, err = r.withRipgrep(target)
		if err != nil {
			return "", err
		}
		identity, _ := ripgrepIdentity(target)
		return launchHash([]any{"hint-docker-study-offline-ripgrep-launch-v1", DockerRunnerVersion, GatePolicy, GatePolicyDigest(), version, target, pin, r.identity(), identity, "uid65534_capdropall_nnp_nohostmount", "root_owned_ancestors", "bounded_env_executable_from_exact_image", "loader_environment_refused"}), nil
	}
	gate := sha256.Sum256([]byte(gatePolicyText))
	if p == "" {
		env := append([]string{}, r.Env...)
		sort.Strings(env)
		return launchHash([]any{"hint-docker-launch-v1", DockerRunnerVersion, GatePolicy, hex.EncodeToString(gate[:]), "opencode", version, target, pin, r.Argv, string(r.Config), env, map[string]any{"user": "65534:65534", "cap_drop": "ALL", "no_new_privileges": true, "tree_owner": "65534:65534", "binary": "root:0755"}}), nil
	}
	return launchHash([]any{"hint-docker-study-launch-v1", DockerRunnerVersion, GatePolicy, hex.EncodeToString(gate[:]), version, target, pin, r.identity(), "uid65534_capdropall_nnp_nohostmount", "root_owned_ancestors", "bounded_env_executable_from_exact_image", "loader_environment_refused"}), nil
}
func openCodeExecutionDigestWithPolicy(model, instruction string, prov map[string]any, imageID string, p StudyLaunchPolicy) (string, error) {
	return openCodeExecutionDigestRuntime(model, instruction, prov, imageID, p, false)
}
func openCodeExecutionDigestRuntime(model, instruction string, prov map[string]any, imageID string, p StudyLaunchPolicy, rg bool) (string, error) {
	h := sha256.Sum256([]byte(instruction))
	r, err := makeOpenCodeRecipe(model, "sha256:"+hex.EncodeToString(h[:]), "<token>", p)
	if err != nil {
		return "", err
	}
	if rg {
		target, _ := prov["target"].(string)
		r, err = r.withRipgrep(target)
		if err != nil {
			return "", err
		}
		if prov["version"] != DockerOpenCodeVersion || prov["ripgrep_sha256"] != ripgrepSHA256 || !prelaunchDigestPattern.MatchString(imageID) {
			return "", errors.New("offline ripgrep execution identity is unverified")
		}
		identity, _ := ripgrepIdentity(target)
		return launchHash([]any{"hint-docker-study-offline-ripgrep-execution-v1", r.identity(), prov["version"], prov["binary_sha256"], target, imageID, identity}), nil
	}
	if p == "" {
		archive, err := r.archive(nil)
		if err != nil {
			return "", err
		}
		cfg := sha256.Sum256(archive)
		return launchHash([]any{r.Argv, hex.EncodeToString(cfg[:]), prov["version"], prov["binary_sha256"], prov["target"]}), nil
	}
	if !prelaunchDigestPattern.MatchString(imageID) {
		return "", errors.New("study launch needs exact image identity")
	}
	return launchHash([]any{"hint-docker-study-execution-v1", r.identity(), prov["version"], prov["binary_sha256"], prov["target"], imageID}), nil
}
