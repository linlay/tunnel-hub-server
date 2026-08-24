package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidModulePath(t *testing.T) {
	for _, value := range []string{
		"example.invalid/tunnel-hub-server",
		"github.com/example/tunnel-hub-server",
		"gitlab.example.test/group/tunnel-hub-server",
	} {
		if !validModulePath(value) {
			t.Fatalf("expected valid module path: %s", value)
		}
	}
	for _, value := range []string{"", "tunnel-hub-server", "https://example.test/tunnel", "example.test//tunnel", "example.test/tunnel/", "example..test/tunnel", "-example.test/tunnel"} {
		if validModulePath(value) {
			t.Fatalf("expected invalid module path: %s", value)
		}
	}
}

func TestResolveModulePathUsesEnvironmentBeforeDotEnv(t *testing.T) {
	t.Setenv(modulePathEnv, "github.com/example/tunnel-hub-server")
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte(modulePathEnv+"=gitlab.example.test/group/tunnel-hub-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveModulePath("", envFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != "github.com/example/tunnel-hub-server" {
		t.Fatalf("module path = %q", got)
	}
}

func TestEnvFilePathPreservesAbsolutePath(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), ".env")
	if got := envFilePath("/repository", absolute); got != absolute {
		t.Fatalf("envFilePath = %q, want %q", got, absolute)
	}
}

func TestRewriteModuleOnlyChangesGoSourcesAndDirective(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.invalid/tunnel-hub-server\n\ngo 1.25.0\n")
	writeTestFile(t, filepath.Join(root, "internal", "sample.go"), "package sample\n\nimport _ \"example.invalid/tunnel-hub-server/internal/other\"\n")
	writeTestFile(t, filepath.Join(root, "README.md"), "example.invalid/tunnel-hub-server\n")

	target := "gitlab.example.test/group/tunnel-hub-server"
	if err := rewriteModule(root, target); err != nil {
		t.Fatal(err)
	}
	assertTestFileContains(t, filepath.Join(root, "go.mod"), "module "+target)
	assertTestFileContains(t, filepath.Join(root, "internal", "sample.go"), target+"/internal/other")
	assertTestFileContains(t, filepath.Join(root, "README.md"), "example.invalid/tunnel-hub-server")
}

func TestPrepareTemporaryTreeIncludesUntrackedSourceAndExcludesLocalFiles(t *testing.T) {
	source := t.TempDir()
	runTestCommand(t, source, "git", "init")
	writeTestFile(t, filepath.Join(source, ".gitignore"), ".env\nbin/\n")
	writeTestFile(t, filepath.Join(source, "go.mod"), "module example.invalid/tunnel-hub-server\n\ngo 1.25.0\n")
	writeTestFile(t, filepath.Join(source, "internal", "tracked.go"), "package internal\n\nimport _ \"example.invalid/tunnel-hub-server/other\"\n")
	writeTestFile(t, filepath.Join(source, "internal", "new.go"), "package internal\n")
	writeTestFile(t, filepath.Join(source, ".env"), "SECRET=do-not-copy\n")
	writeTestFile(t, filepath.Join(source, "bin", "relay"), "build output\n")

	target := "github.com/example/tunnel-hub-server"
	temporary, cleanup, err := prepareTemporaryTree(source, target)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	assertTestFileContains(t, filepath.Join(temporary, "go.mod"), "module "+target)
	assertTestFileContains(t, filepath.Join(temporary, "internal", "tracked.go"), target+"/other")
	assertTestFileContains(t, filepath.Join(temporary, "internal", "new.go"), "package internal")
	for _, excluded := range []string{".env", filepath.Join("bin", "relay")} {
		if _, err := os.Stat(filepath.Join(temporary, excluded)); !os.IsNotExist(err) {
			t.Fatalf("excluded file copied to temporary tree: %s", excluded)
		}
	}
	assertTestFileContains(t, filepath.Join(source, "go.mod"), "module example.invalid/tunnel-hub-server")
}

func TestExcludedWorktreePath(t *testing.T) {
	for _, path := range []string{".env", ".local/runtime.json", "bin/relay", "web/node_modules/pkg/index.js", "web/dist/index.js", "output/build/app", "configs/key.pem", "data/local.sqlite"} {
		if !excludedWorktreePath(path) {
			t.Fatalf("expected excluded path: %s", path)
		}
	}
	if excludedWorktreePath("internal/config/config.go") {
		t.Fatal("Go source must not be excluded")
	}
}

func TestCopyFileRejectsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	writeTestFile(t, target, "secret")
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(link, filepath.Join(t.TempDir(), "link")); err == nil {
		t.Fatal("symbolic link was copied into the temporary tree")
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTestFileContains(t *testing.T, path, wanted string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), wanted) {
		t.Fatalf("%s does not contain %q", path, wanted)
	}
}

func runTestCommand(t *testing.T, directory, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, output)
	}
}
