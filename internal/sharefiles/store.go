package sharefiles

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var controlledID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var ErrTooLarge = errors.New("conversation share resource is too large")

type Store struct {
	root string
}

type Stage struct {
	store *Store
	id    string
	path  string
	done  bool
}

type WrittenFile struct {
	Size   int64
	SHA256 string
}

func New(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("conversation share resource directory must be absolute")
	}
	root = filepath.Clean(root)
	for _, directory := range []string{root, filepath.Join(root, ".staging"), filepath.Join(root, "shares")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create conversation share resource directory: %w", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("conversation share resource directory must not contain managed symbolic links")
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("secure conversation share resource directory: %w", err)
		}
	}
	probe, err := os.CreateTemp(root, ".write-probe-")
	if err != nil {
		return nil, fmt.Errorf("conversation share resource directory is not writable: %w", err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		return nil, err
	}
	if err := os.Remove(probeName); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Begin() (*Stage, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:])
	path := filepath.Join(s.root, ".staging", id)
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	return &Stage{store: s, id: id, path: path}, nil
}

func (stage *Stage) Write(resourceID string, source io.Reader, maximum int64) (WrittenFile, error) {
	if stage == nil || stage.done || !controlledID.MatchString(resourceID) || maximum < 0 {
		return WrittenFile{}, errors.New("invalid staged conversation resource")
	}
	file, err := os.OpenFile(filepath.Join(stage.path, resourceID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return WrittenFile{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, maximum+1))
	if copyErr == nil && written > maximum {
		copyErr = ErrTooLarge
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(filepath.Join(stage.path, resourceID))
		return WrittenFile{}, copyErr
	}
	return WrittenFile{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (stage *Stage) Commit(shareID string) error {
	if stage == nil || stage.done || !controlledID.MatchString(shareID) {
		return errors.New("invalid conversation share resource commit")
	}
	sharesRoot := filepath.Join(stage.store.root, "shares")
	parent := filepath.Join(sharesRoot, shareID)
	if err := os.Mkdir(parent, 0o700); err != nil {
		return err
	}
	if err := syncDirectory(sharesRoot); err != nil {
		_ = os.Remove(parent)
		return err
	}
	if err := syncDirectory(stage.path); err != nil {
		_ = os.Remove(parent)
		return err
	}
	if err := os.Rename(stage.path, filepath.Join(parent, "resources")); err != nil {
		_ = os.Remove(parent)
		return err
	}
	stage.done = true
	return syncDirectory(parent)
}

func (stage *Stage) Abort() error {
	if stage == nil || stage.done {
		return nil
	}
	stage.done = true
	return os.RemoveAll(stage.path)
}

func (s *Store) Open(shareID, resourceID string) (*os.File, error) {
	if !controlledID.MatchString(shareID) || !controlledID.MatchString(resourceID) {
		return nil, os.ErrNotExist
	}
	parts := []string{"shares", shareID, "resources", resourceID}
	path := s.root
	for index, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrNotExist
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, os.ErrNotExist
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, os.ErrNotExist
		}
	}
	return os.Open(path)
}

func (s *Store) Verify(shareID, resourceID string, size int64, sha256Hex string) error {
	file, err := s.Open(shareID, resourceID)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		return errors.New("conversation share resource size mismatch")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(sha256Hex) {
		return errors.New("conversation share resource hash mismatch")
	}
	return nil
}

func (s *Store) Delete(shareID string) error {
	if !controlledID.MatchString(shareID) {
		return os.ErrNotExist
	}
	return os.RemoveAll(filepath.Join(s.root, "shares", shareID))
}

func (s *Store) CleanupOrphans(validShareIDs map[string]struct{}, before time.Time) error {
	if err := cleanupOldChildren(filepath.Join(s.root, ".staging"), before, nil); err != nil {
		return err
	}
	return cleanupOldChildren(filepath.Join(s.root, "shares"), before, validShareIDs)
}

func cleanupOldChildren(parent string, before time.Time, keep map[string]struct{}) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if keep != nil {
			if _, ok := keep[entry.Name()]; ok {
				continue
			}
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(before) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
