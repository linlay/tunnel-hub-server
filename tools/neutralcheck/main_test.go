package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSplitTermsNormalizesAndDeduplicates(t *testing.T) {
	terms := splitTerms(" Alpha, beta\nALPHA ")
	if len(terms) != 2 || !bytes.Equal(terms[0], []byte("alpha")) || !bytes.Equal(terms[1], []byte("beta")) {
		t.Fatalf("terms = %q", terms)
	}
}

func TestEnvFilePathPreservesAbsolutePath(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), ".env")
	if got := envFilePath("/repository", absolute); got != absolute {
		t.Fatalf("envFilePath = %q, want %q", got, absolute)
	}
}

func TestScanIncludesUntrackedFilesAndSkipsDeletedTrackedFiles(t *testing.T) {
	root := t.TempDir()
	runTestCommand(t, root, "git", "init")
	deletedPath := filepath.Join(root, "blocked-path.txt")
	if err := os.WriteFile(deletedPath, []byte("neutral"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestCommand(t, root, "git", "add", "blocked-path.txt")
	if err := os.Remove(deletedPath); err != nil {
		t.Fatal(err)
	}
	if err := scan(root, "", splitTerms("blocked")); err != nil {
		t.Fatalf("deleted tracked file should be skipped: %v", err)
	}

	untrackedPath := filepath.Join(root, "new.go")
	if err := os.WriteFile(untrackedPath, []byte("package sample // blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scan(root, "", splitTerms("blocked")); err == nil {
		t.Fatal("untracked forbidden content was not detected")
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
