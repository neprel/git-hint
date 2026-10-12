package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git-hint")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Join(wd, "..", "..")); err != nil { // docFiles are repository files
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	when := time.Unix(1700000000, 0).UTC()
	lic := []namedFile{{"licenses/go/LICENSE", []byte("license")}}
	a, b := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "b.tar.gz")
	if err := writeArchive(a, "x", bin, lic, when); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := writeArchive(b, "x", bin, lic, when); err != nil {
		t.Fatal(err)
	}
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	if !bytes.Equal(ba, bb) {
		t.Fatal("two archives of the same input differ")
	}
}

func TestCheckBinaryFindsForbiddenContent(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"fixture":   "x HINTCANARY_SRC_5e1 y",
		"home":      "path /Users/someone/project/x.go",
		"lan":       "dial 10.20.30.40:8000",
		"pat":       "ghp_abcdefghijklmnopqrstuvwxyz0123",
		"key":       "-----BEGIN OPENSSH PRIVATE KEY-----",
		"go source": "package billing\n",
	} {
		p := filepath.Join(dir, "bad")
		_ = os.WriteFile(p, []byte(body), 0o644)
		if len(checkBinary(p)) == 0 {
			t.Errorf("%s: not detected", name)
		}
	}
	ok := filepath.Join(dir, "ok")
	// the validator's bare markers and public addresses are allowed
	_ = os.WriteFile(ok, []byte("HINTCANARY_ canary_dir_e1d2 canarySymbolQ7 127.0.0.1 /home/ 8.8.8.8"), 0o644)
	if bad := checkBinary(ok); len(bad) != 0 {
		t.Errorf("false positive: %v", bad)
	}
}

func TestSumsAndVerify(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "git-hint_1.0.0_linux_amd64.tar.gz"), []byte("a"), 0o644)
	sum, size, _ := hashFile(filepath.Join(dir, "git-hint_1.0.0_linux_amd64.tar.gz"))
	m := Manifest{Artifacts: []Artifact{{OS: "linux", Arch: "amd64", File: "git-hint_1.0.0_linux_amd64.tar.gz", SHA256: sum, Size: size}}}
	mb, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	if err := writeSums(dir); err != nil {
		t.Fatal(err)
	}
	if err := cmdVerify([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "git-hint_1.0.0_linux_amd64.tar.gz"), []byte("b"), 0o644)
	if err := cmdVerify([]string{"-dir", dir}); err == nil {
		t.Fatal("a changed archive passed")
	}
}

func TestVersionAndTargetRules(t *testing.T) {
	for v, ok := range map[string]bool{"v0.1.0": true, "v1.2.3-rc.1": true, "v0.0.0-dev": true, "v0.0.0-ci": false, "v1.0.0-rc.01": false,
		"0.1.0": false, "v1.2": false, "v01.2.3": false, "v1.2.3;rm": false} {
		if semver.MatchString(v) != ok {
			t.Errorf("%s: want %v", v, ok)
		}
	}
	if err := cmdBuild([]string{"-version", "v0.1.0", "-out", filepath.Join(t.TempDir(), "d"), "-targets", "windows/amd64", "-allow-dirty"}); err == nil || !strings.Contains(err.Error(), "not a release target") {
		t.Fatalf("a Windows target: %v", err)
	}
}

func TestVersionPatternMatchesSchema(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0.1", "common.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for name, d := range s.Defs {
		if strings.Contains(d.Pattern, "alpha|beta|rc|dev") {
			if "^v"+strings.TrimPrefix(d.Pattern, "^") != semver.String() {
				t.Fatalf("release version pattern %s differs from schema %s pattern %s", semver, name, d.Pattern)
			}
			return
		}
	}
	t.Fatal("no version pattern in the schema")
}
