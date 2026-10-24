package runner

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const studyInspectionBytes int64 = 2 << 20
const studyInspectionEntries = 128
const studyInspectionBound = 30 * time.Second

const studyForbiddenMode int64 = 07022
const studyAncestorRequiredMode int64 = 0501
const studyLauncherRequiredMode int64 = 0001

func studyRequiredAncestors() []string { return []string{"/", "/usr", "/usr/bin", "/opt"} }
func studyOptionalAncestors() []string { return []string{"/opt/hint", "/opt/hint/bin"} }
func studyAbsentPaths() []string       { return []string{studyRoot, "/opt/hint/bin/opencode"} }
func studyLoaderPrefixes() []string    { return []string{"LD_", "DYLD_"} }
func studyPAXKeys() []string           { return []string{"path", "mtime", "atime", "ctime"} }
func studyInspectionRules() any {
	return map[string]any{
		"version": "single-root-inspection-v1", "required_ancestors": studyRequiredAncestors(), "optional_ancestors": studyOptionalAncestors(), "absent_paths": studyAbsentPaths(), "launcher": studyLauncher,
		"uid": 0, "gid": 0, "forbidden_mode_bits": studyForbiddenMode, "ancestor_required_mode": studyAncestorRequiredMode, "launcher_required_mode": studyLauncherRequiredMode,
		"loader_prefixes": studyLoaderPrefixes(), "pax_keys": studyPAXKeys(), "no_links_or_xattrs": true, "entries": studyInspectionEntries, "raw_bytes": studyInspectionBytes, "timeout_ms": studyInspectionBound.Milliseconds(),
		"capture_entries": maxTreeFiles, "capture_content_bytes": maxTreeBytes, "capture_raw_bytes": maxPrelaunchArchiveBytes, "capture_timeout_ms": prelaunchCaptureBound.Milliseconds(), "tar_completion": "two_end_blocks_zero_file_record_padding_v1",
	}
}

// StudyLaunchReceipt is local inspection evidence only. Prepared never implies binary
// correctness, source correspondence, no children, or producer/calibration eligibility.
type StudyLaunchReceipt struct {
	Policy           StudyLaunchPolicy `json:"policy"`
	State            string            `json:"state"`
	Clock            string            `json:"clock"`
	StartedMs        *int64            `json:"started_ms"`
	FinishedMs       *int64            `json:"finished_ms"`
	LaunchDigest     string            `json:"launch_digest"`
	ExecutionDigest  string            `json:"execution_digest"`
	ImageID          string            `json:"image_id"`
	LauncherDigest   string            `json:"launcher_digest,omitempty"`
	InventoryDigest  string            `json:"inventory_digest,omitempty"`
	BinaryValidation string            `json:"binary_validation"`
	Eligibility      string            `json:"eligibility"`
}

type studyEntry struct {
	Name     string
	Type     byte
	Mode     int64
	Size     int64
	UID, GID int
	Digest   string
}

func safeStudyDir(h studyEntry) bool {
	return h.Type == tar.TypeDir && h.UID == 0 && h.GID == 0 && h.Mode&studyForbiddenMode == 0 && h.Mode&studyAncestorRequiredMode == studyAncestorRequiredMode
}

// studyEntries intentionally reads only a bounded first header for ancestor metadata.
// Full inventories/executables require a complete bounded archive and no aliases.
func (d *Docker) studyEntries(ctx context.Context, id, source string, first bool) ([]studyEntry, error) {
	res, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: source})
	if err != nil {
		return nil, err
	}
	defer res.Content.Close()
	if res.Stat.LinkTarget != "" {
		return nil, errors.New("study path is a link")
	}
	raw := &treeArchiveReader{source: res.Content, limit: studyInspectionBytes}
	tr := tar.NewReader(raw)
	var entries []studyEntry
	seen := map[string]bool{}
	var padding int64
	root := path.Base(source)
	for {
		before := raw.read
		raw.padding = padding
		h, err := tr.Next()
		if err == io.EOF {
			if err := raw.finish(before, padding); err != nil {
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, err
		}
		padding = (tarBlockBytes - h.Size%tarBlockBytes) % tarBlockBytes
		for key := range h.PAXRecords {
			allowed := false
			for _, known := range studyPAXKeys() {
				allowed = allowed || key == known
			}
			if !allowed {
				return nil, errors.New("unreviewed study archive metadata")
			}
		}
		name := strings.TrimSuffix(h.Name, "/")
		if source == "/" && (name == "." || h.Name == "/") {
			name = ""
		} else {
			if name == root {
				name = ""
			} else if strings.HasPrefix(name, root+"/") {
				name = strings.TrimPrefix(name, root+"/")
			} else {
				return nil, errors.New("study archive root mismatch")
			}
		}
		if name != "" {
			if _, ok := treePath("app/" + name); !ok {
				return nil, errors.New("study archive path is not canonical")
			}
		}
		if seen[name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || len(h.Xattrs) > 0 || len(entries) >= studyInspectionEntries {
			return nil, errors.New("ambiguous study archive")
		}
		if h.Size < 0 || h.Size > studyInspectionBytes {
			return nil, errors.New("study archive content too large")
		}
		seen[name] = true
		e := studyEntry{Name: name, Type: h.Typeflag, Mode: h.Mode, Size: h.Size, UID: h.Uid, GID: h.Gid}
		if first {
			if name != "" || h.Size != 0 || !safeStudyDir(e) {
				return nil, errors.New("unsafe study ancestor")
			}
			return []studyEntry{e}, nil
		}
		if h.Typeflag == tar.TypeReg {
			hash := sha256.New()
			if _, err := io.Copy(hash, tr); err != nil {
				return nil, err
			}
			e.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (d *Docker) inspectStudyContainer(ctx context.Context, id, image string, r openCodeRecipe) error {
	ins, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return errors.New("study container inspect failed")
	}
	c := ins.Container
	if c.Image != image || !prelaunchDigestPattern.MatchString(image) || c.State == nil || c.State.Status != "created" || c.State.Running || c.State.Paused || c.State.Restarting || c.State.Dead || c.State.Pid != 0 || c.Config == nil || c.Config.User != "65534:65534" || c.Config.WorkingDir != "/app" || len(c.Config.Entrypoint) != 0 || !reflect.DeepEqual([]string(c.Config.Cmd), r.Argv) || len(c.Mounts) != 0 || c.HostConfig == nil || noHostAccess(c.HostConfig) != nil {
		return errors.New("study container does not match launch recipe")
	}
	env := map[string]string{}
	for _, item := range c.Config.Env {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return errors.New("ambiguous study container environment")
		}
		if _, exists := env[key]; exists {
			return errors.New("duplicate study container environment")
		}
		env[key] = value
		loader := false
		for _, prefix := range studyLoaderPrefixes() {
			loader = loader || strings.HasPrefix(key, prefix)
		}
		if value != "" && loader {
			return errors.New("study environment launcher has loader injection settings")
		}
	}
	for _, item := range r.Env {
		key, value, _ := strings.Cut(item, "=")
		if got, ok := env[key]; !ok || got != value {
			return errors.New("study environment does not match recipe")
		}
	}
	return nil
}

func (d *Docker) inspectStudyBefore(ctx context.Context, id, image string, r openCodeRecipe) (string, error) {
	if err := d.inspectStudyContainer(ctx, id, image, r); err != nil {
		return "", err
	}
	// Parent metadata comes before any child path. Absence is accepted only after
	// successful archive capability/ancestor checks; inconclusive errors always refuse.
	for _, source := range studyRequiredAncestors() {
		if _, err := d.studyEntries(ctx, id, source, true); err != nil {
			return "", err
		}
	}
	missing := false
	for _, source := range studyOptionalAncestors() {
		_, err := d.studyEntries(ctx, id, source, true)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				missing = true
				continue
			}
			return "", err
		}
		if missing {
			return "", errors.New("inconsistent study ancestor state")
		}
	}
	for _, source := range studyAbsentPaths() {
		res, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: source})
		if err == nil {
			res.Content.Close()
			return "", errors.New("study install path already exists")
		}
		if !cerrdefs.IsNotFound(err) {
			return "", errors.New("study install path absence is ambiguous")
		}
	}
	entries, err := d.studyEntries(ctx, id, studyLauncher, false)
	if err != nil || len(entries) != 1 {
		return "", errors.New("study environment launcher unavailable")
	}
	e := entries[0]
	if e.Name != "" || e.Type != tar.TypeReg || e.Size <= 0 || e.UID != 0 || e.GID != 0 || e.Mode&studyLauncherRequiredMode != studyLauncherRequiredMode || e.Mode&studyForbiddenMode != 0 || !prelaunchDigestPattern.MatchString(e.Digest) {
		return "", errors.New("unsafe study environment launcher")
	}
	return e.Digest, nil
}
func (d *Docker) inspectStudyAfter(ctx context.Context, id, image string, r openCodeRecipe) (string, error) {
	if err := d.inspectStudyContainer(ctx, id, image, r); err != nil {
		return "", err
	}
	got, err := d.studyEntries(ctx, id, studyRoot, false)
	if err != nil {
		return "", err
	}
	var expected []studyEntry
	prefix := strings.TrimPrefix(studyRoot, "/")
	for _, f := range r.Files {
		name := strings.TrimSuffix(f.Name, "/")
		if name != prefix && !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, prefix), "/")
		e := studyEntry{Name: name, Type: f.Type, Mode: f.Mode, Size: int64(len(f.Body)), UID: f.UID, GID: f.GID}
		if f.Type == tar.TypeReg {
			h := sha256.Sum256([]byte(f.Body))
			e.Digest = "sha256:" + hex.EncodeToString(h[:])
		}
		expected = append(expected, e)
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })
	if !reflect.DeepEqual(got, expected) {
		return "", errors.New("study configuration or initial state differs from recipe")
	}
	return launchHash(got), nil
}
