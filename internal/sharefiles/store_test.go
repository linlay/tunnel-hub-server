package sharefiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStageCommitFreezesResourcesByShare(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	written, err := stage.Write("0123456789abcdef01234567", bytes.NewReader([]byte("resource")), 20)
	if err != nil {
		t.Fatal(err)
	}
	if written.Size != 8 {
		t.Fatalf("written=%+v", written)
	}
	if err := stage.Commit("share_first"); err != nil {
		t.Fatal(err)
	}
	file, err := store.Open("share_first", "0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%o", info.Mode().Perm())
	}
	if _, err := store.Open("share_second", "0123456789abcdef01234567"); !os.IsNotExist(err) {
		t.Fatalf("cross-share open err=%v", err)
	}
	second, err := store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Write("0123456789abcdef01234567", bytes.NewReader([]byte("second")), 20); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit("share_second"); err != nil {
		t.Fatal(err)
	}
	firstBody, err := os.ReadFile(filepath.Join(store.Root(), "shares", "share_first", "resources", "0123456789abcdef01234567"))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := os.ReadFile(filepath.Join(store.Root(), "shares", "share_second", "resources", "0123456789abcdef01234567"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != "resource" || string(secondBody) != "second" {
		t.Fatalf("share resources are not independent: first=%q second=%q", firstBody, secondBody)
	}
	for _, directory := range []string{store.Root(), filepath.Join(store.Root(), "shares"), filepath.Join(store.Root(), "shares", "share_first"), filepath.Join(store.Root(), "shares", "share_first", "resources"), filepath.Join(store.Root(), "shares", "share_second", "resources")} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s mode=%o", directory, info.Mode().Perm())
		}
	}
}

func TestStageWriteTooLargeLeavesNoPartialResource(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if _, err := stage.Write("0123456789abcdef01234567", bytes.NewReader([]byte("oversized")), 3); err != ErrTooLarge {
		t.Fatalf("write err=%v", err)
	}
	entries, err := os.ReadDir(stage.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staged files=%v", entries)
	}
}

func TestStageCommitConflictDoesNotExposePartialShare(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if _, err := stage.Write("0123456789abcdef01234567", bytes.NewReader([]byte("resource")), 20); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(store.Root(), "shares", "share_conflict")
	if err := os.WriteFile(conflict, []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit("share_conflict"); err == nil {
		t.Fatal("expected commit conflict")
	}
	if _, err := store.Open("share_conflict", "0123456789abcdef01234567"); !os.IsNotExist(err) {
		t.Fatalf("partial share became readable: %v", err)
	}
}

func TestOpenRejectsSymbolicLink(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	resourceDir := filepath.Join(store.Root(), "shares", "share_link", "resources")
	if err := os.MkdirAll(resourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(resourceDir, "0123456789abcdef01234567")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("share_link", "0123456789abcdef01234567"); !os.IsNotExist(err) {
		t.Fatalf("symbolic link open err=%v", err)
	}
}

func TestOpenRejectsSymbolicLinkInResourceDirectory(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "resources")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	const resourceID = "0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(target, resourceID), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	shareDir := filepath.Join(store.Root(), "shares", "share_link")
	if err := os.MkdirAll(shareDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(shareDir, "resources")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open("share_link", resourceID); !os.IsNotExist(err) {
		t.Fatalf("symbolic resource directory open err=%v", err)
	}
}

func TestNewRejectsManagedSymbolicLink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "conversation-shares")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, ".staging")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root); err == nil {
		t.Fatal("managed symbolic link accepted")
	}
}

func TestCleanupOrphansKeepsDatabaseOwnedShares(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "conversation-shares"))
	if err != nil {
		t.Fatal(err)
	}
	staleStage, err := store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer staleStage.Abort()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleStage.path, old, old); err != nil {
		t.Fatal(err)
	}
	for _, shareID := range []string{"share_owned", "share_orphan"} {
		stage, err := store.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := stage.Commit(shareID); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Root(), "shares", shareID)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CleanupOrphans(map[string]struct{}{"share_owned": {}}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "shares", "share_owned")); err != nil {
		t.Fatalf("owned share was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "shares", "share_orphan")); !os.IsNotExist(err) {
		t.Fatalf("orphan share still exists: %v", err)
	}
	if _, err := os.Stat(staleStage.path); !os.IsNotExist(err) {
		t.Fatalf("stale staging directory still exists: %v", err)
	}
}
