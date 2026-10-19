package shellclass

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestUnknownShellCommandsRemainUnknown: unsupported syntax and commands without a rule
// stay UNKNOWN.
func TestUnknownShellCommandsRemainUnknown(t *testing.T) {
	cases := map[string]string{
		// Syntax that the parser rejects.
		"parse error: unterminated quote":    `echo 'unterminated`,
		"parse error: bad if":                `if then fi`,
		"parse error: unclosed subshell":     `(cd src && ls`,
		"parse error: unclosed substitution": `cat $(ls`,
		"parse error: zsh glob qualifier":    `print -l ${(f)"$(ls)"}`,
		// Syntax that the walker does not support.
		"coprocess": `coproc cat big.log`,
		// Commands with no rule.
		"unknown program": `hintctl sync --all`,
		"alias ll":        `ll src`,
		"alias gst":       `gst`,
		"unknown in pipe": `foo | bar`,
		// Dynamic or opaque execution.
		"dynamic command name":  `$TOOL --fix`,
		"eval":                  `eval "$CMD"`,
		"source":                `source ./env.sh`,
		"opaque script":         `./scripts/bootstrap.sh`,
		"make target":           `make lint`,
		"npm script":            `npm run build`,
		"python -c":             `python3 -c "open('x','w').write('1')"`,
		"node -e":               `node -e "require('fs').unlinkSync('a')"`,
		"perl without -i":       `perl -e 'unlink "a"'`,
		"dynamic sh -c":         `bash -c "$BUILD_CMD"`,
		"nested sh -c too deep": `bash -c 'bash -c "cat x"'`,
		"unknown git sub":       `git frobnicate`,
		"interactive editor":    `vim main.go`,
		"input too long":        "echo " + strings.Repeat("a", MaxInputBytes),
	}
	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			r := Classify(cmd)
			if !slices.Contains(r.Labels, UNKNOWN) {
				t.Fatalf("Classify(%q) = %v, want UNKNOWN in labels", cmd, r.Labels)
			}
			if r.Op != UNKNOWN && r.Op != MIXED {
				t.Fatalf("Classify(%q).Op = %s, want UNKNOWN or MIXED", cmd, r.Op)
			}
			if slices.Contains(r.Labels, OTHER) {
				t.Fatalf("Classify(%q) collapsed to OTHER: %v", cmd, r.Labels)
			}
			if len(r.Reasons) == 0 {
				t.Fatalf("Classify(%q) has no reason for UNKNOWN", cmd)
			}
		})
	}
}

// TestUnknownIsNotDroppedByKnownParts: one unknown part keeps UNKNOWN even
// when other parts of the command are known.
func TestUnknownIsNotDroppedByKnownParts(t *testing.T) {
	for _, cmd := range []string{
		`cat a.txt && ./run.sh`,
		`git status; mytool`,
		`rg -l x | xargs mytool`,
		`ls $(customcmd)`,
	} {
		r := Classify(cmd)
		if !slices.Contains(r.Labels, UNKNOWN) {
			t.Errorf("Classify(%q) = %v, want UNKNOWN kept", cmd, r.Labels)
		}
	}
}

// TestClassifierNeverExecutes checks that classification has no side
// effect for commands that would create or delete files.
func TestClassifierNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	made := filepath.Join(dir, "made.txt")
	for _, cmd := range []string{
		"touch " + made,
		"echo hi > " + made,
		"rm -f " + keep,
		"bash -c 'touch " + made + "; rm " + keep + "'",
		"python3 -c \"open('" + made + "','w')\"",
		"x=$(touch " + made + ")",
	} {
		Classify(cmd)
	}
	if _, err := os.Stat(made); !os.IsNotExist(err) {
		t.Fatalf("classification created %s", made)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("classification removed %s: %v", keep, err)
	}
}

func TestKnownCommands(t *testing.T) {
	cases := []struct {
		cmd           string
		labels        []string
		reads, writes []string
	}{
		{`sed -n '1,80p' a.go`, []string{READ}, []string{"a.go"}, nil},
		{`sed -i '' 's/a/b/' x.txt`, []string{READ, WRITE}, []string{"x.txt"}, []string{"x.txt"}},
		{`rg -n foo src`, []string{SEARCH}, []string{"src"}, nil},
		{`go test ./... 2>&1 | tee t.log`, []string{TEST, WRITE}, nil, []string{"t.log"}},
		{`cat > a.go <<'EOF'` + "\npackage a\nEOF", []string{WRITE}, nil, []string{"a.go"}},
		{`rm -rf dist build`, []string{DELETE}, nil, []string{"dist", "build"}},
		{`bash -lc "cat package.json"`, []string{READ}, []string{"package.json"}, nil},
		{"apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: a.go\n*** Delete File: b.go\n*** End Patch\nEOF",
			[]string{DELETE, WRITE}, nil, []string{"a.go", "b.go"}},
		{"git apply <<'EOF'\n--- a/x/y.go\n+++ b/x/y.go\n@@ -1 +1 @@\n-a\n+b\nEOF",
			[]string{VCS, WRITE}, nil, []string{"x/y.go"}},
		{`echo hi`, []string{OTHER}, nil, nil},
	}
	for _, c := range cases {
		r := Classify(c.cmd)
		want := slices.Clone(c.labels)
		slices.Sort(want)
		if !slices.Equal(r.Labels, want) {
			t.Errorf("Classify(%q).Labels = %v, want %v", c.cmd, r.Labels, want)
		}
		if c.reads != nil && !slices.Equal(r.ReadTargets, c.reads) {
			t.Errorf("Classify(%q).ReadTargets = %v, want %v", c.cmd, r.ReadTargets, c.reads)
		}
		if c.writes != nil && !slices.Equal(r.WriteTargets, c.writes) {
			t.Errorf("Classify(%q).WriteTargets = %v, want %v", c.cmd, r.WriteTargets, c.writes)
		}
		if len(r.Labels) > 1 && r.Op != MIXED {
			t.Errorf("Classify(%q).Op = %s, want MIXED", c.cmd, r.Op)
		}
	}
}

// sample is a small synthetic command list for the no-panic and benchmark checks.
var sample = []string{
	"cat internal/api/handler.go", "sed -n '1,20p' a.go", "grep -rn Foo src", "go test ./...", "npm run build",
	"rm -rf build", "git status", "ls -la", "find . -name '*.go' | xargs wc -l", "echo hi > out.txt",
	"bash -c 'cat a | grep b'", "python3 -m pytest -q", "curl -s https://example.invalid", "foo | bar",
}

// TestSampleNoPanic runs every sample command once.
func TestSampleNoPanic(t *testing.T) {
	for _, c := range sample {
		if r := Classify(c); len(r.Labels) == 0 {
			t.Fatalf("no labels for %q", c)
		}
	}
}

func BenchmarkClassifySample(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Classify(sample[i%len(sample)])
	}
}

// Content-printing git commands are a read; a proven suppression is not; unknown option
// combinations keep the read. No target is resolved from revisions or blob paths.
func TestGitContentReads(t *testing.T) {
	read := []string{`git show HEAD`, `git show HEAD:internal/a.go`, `git show --stat -p HEAD`, `git show -s HEAD:a.go`, `git show --format=%H HEAD`,
		`git show -sp HEAD`, `git diff`, `git diff --exit-code`, `git diff --stat -p`, `git diff HEAD~1 -- a.go`, `git log -p`, `git log --oneline -p -3`,
		`git log -pw`, `git log --patch-with-stat`, `git whatchanged -p`, `git blame a.go`, `git annotate a.go`, `git cat-file -p HEAD:a.go`,
		`git cat-file blob abc123`, `git range-diff a...b`, `git -C sub show HEAD`, `git --no-pager diff --cached`}
	noRead := []string{`git status`, `git log`, `git log --oneline -3`, `git log -S needle`, `git log -p --no-patch`, `git show -s HEAD`,
		`git show --no-patch --format=%H HEAD`, `git show --stat HEAD`, `git show --name-only HEAD`, `git show --quiet HEAD`, `git diff --stat`,
		`git diff --name-only`, `git diff --name-status HEAD~1`, `git diff --quiet`, `git diff --shortstat`, `git cat-file -t HEAD`,
		`git cat-file -s HEAD`, `git branch`, `git range-diff --no-patch a...b`}
	for _, c := range read {
		if r := Classify(c); !slices.Contains(r.Labels, READ) || !slices.Contains(r.Labels, VCS) || len(r.ReadTargets) != 0 {
			t.Errorf("%q: %v reads %v, want VCS+READ without targets", c, r.Labels, r.ReadTargets)
		}
	}
	for _, c := range noRead {
		if r := Classify(c); slices.Contains(r.Labels, READ) {
			t.Errorf("%q: %v, want no READ", c, r.Labels)
		}
	}
}

// C7BE-29: the patch switches count in order (the last wins) and nothing after "--" is an option.
// Every row was checked against the output of git 2.56 in a temporary repository.
func TestGitPatchOrderAndOptionBoundary(t *testing.T) {
	read := []string{`git log --no-patch -p -1`, `git log --no-patch --patch -1`, `git log -s -p`, `git diff -- --stat`, `git diff -- --name-only a.go`,
		`git show --no-patch -p HEAD`, `git show -s --patch HEAD`, `git show --quiet -p HEAD`, `git show --patch --quiet HEAD`, `git diff --no-patch -p`, `git show -- --stat`,
		`git range-diff --no-patch -p a...b`}
	noRead := []string{`git log -p --no-patch -1`, `git log --patch -s`, `git show -p --no-patch HEAD`,
		`git diff -p --no-patch`, `git diff --stat -- -p`}
	for _, c := range read {
		if r := Classify(c); !slices.Contains(r.Labels, READ) {
			t.Errorf("%q: %v, want READ", c, r.Labels)
		}
	}
	if r := Classify(`git log -- -p`); slices.Contains(r.Labels, READ) { // "-p" is a path here; plain log prints no patch
		t.Errorf("git log -- -p: %v", r.Labels)
	}
	for _, c := range noRead {
		if r := Classify(c); slices.Contains(r.Labels, READ) {
			t.Errorf("%q: %v, want no READ", c, r.Labels)
		}
	}
}
