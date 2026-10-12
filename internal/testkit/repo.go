package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Canaries are fake secrets and names placed in fixture repositories and agent events.
// None of them may appear in an export package or an API request.
var Canaries = []string{
	"HINTCANARY_", "canary_dir_e1d2", "canarySymbolQ7", "canary-branch-x9", "canary@example.invalid",
	"CanaryLedgerK3", "canary-remote.invalid",
}

// Git runs git in dir with a fixed identity and fails the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=HINTCANARY_author", "-c",
		"user.email=canary@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// WriteFile writes a file below dir, creating directories.
func WriteFile(t testing.TB, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Fixture creates two linked repositories in a temporary directory: "shop" (Go service,
// TypeScript, HTML, SCSS, Markdown, YAML) and "shop-web" (TypeScript client). Names,
// contents, the commit message, the branch and the remote hold canary values.
func Fixture(t testing.TB) (shop, web string) {
	t.Helper()
	root := t.TempDir()
	shop = filepath.Join(root, "shop")
	web = filepath.Join(root, "shop-web")
	files := map[string]string{
		"go.mod":                                 "module example.invalid/shop\n\ngo 1.22\n",
		"cmd/shop/main.go":                       "package main\n\nimport \"example.invalid/shop/canary_dir_e1d2/billing\"\n\nfunc main() { _ = billing.Rate(1, 2) }\n",
		"canary_dir_e1d2/billing/ledger.go":      "package billing\n\n// HINTCANARY_COMMENT secret token sk-HINTCANARY_TOKEN\nconst apiKey = \"HINTCANARY_KEY_0123\"\n\n// Rate computes a rate.\nfunc Rate(a, b int) int {\n\tif a > b {\n\t\treturn a\n\t}\n\treturn b\n}\n\ntype CanaryLedgerK3 struct{ total int }\n\nfunc (l *CanaryLedgerK3) Apply(x int) { l.total += x }\n\nfunc canarySymbolQ7() string { return \"HINTCANARY_LITERAL\" }\n",
		"canary_dir_e1d2/billing/ledger_test.go": "package billing\n\nimport \"testing\"\n\nfunc TestRate(t *testing.T) {\n\tif Rate(1, 2) != 2 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n",
		"web/app.ts":                             "export function canarySymbolQ7(): string { return 'HINTCANARY_TS' }\n",
		"web/index.html":                         "<html><body>HINTCANARY_HTML</body></html>\n",
		"web/style.scss":                         ".canary_dir_e1d2 { color: red; }\n",
		"docs/HINTCANARY_doc.md":                 "# HINTCANARY heading\n\nText.\n",
		"deploy/config.yaml":                     "secret: HINTCANARY_YAML\n",
		".env.example":                           "HINT_CANARY_ENV=HINTCANARY_ENV_VALUE\n",
	}
	for rel, c := range files {
		WriteFile(t, shop, rel, c)
	}
	Git(t, shop, "init", "-q")
	Git(t, shop, "checkout", "-q", "-b", "canary-branch-x9")
	Git(t, shop, "remote", "add", "origin", "https://canary-remote.invalid/HINTCANARY_org/shop.git")
	Git(t, shop, "add", "-A")
	Git(t, shop, "commit", "-q", "-m", "HINTCANARY_COMMIT message with canarySymbolQ7")
	WriteFile(t, web, "src/client.ts", "import { canarySymbolQ7 } from '../../shop/web/app'\nexport const x = canarySymbolQ7()\n")
	WriteFile(t, web, "README.md", "# shop-web HINTCANARY_README\n")
	Git(t, web, "init", "-q")
	Git(t, web, "add", "-A")
	Git(t, web, "commit", "-q", "-m", "HINTCANARY_COMMIT web")
	return shop, web
}
