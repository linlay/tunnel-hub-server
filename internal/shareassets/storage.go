package shareassets

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

type templateStore struct {
	writeMu   sync.Mutex
	mu        sync.RWMutex
	root      string
	current   string
	active    *Bundle
	bundles   map[string]*Bundle
	inventory map[string]Version
	err       error
	// Narrow durability test seam; configured before concurrent use.
	before func(string) error
}

// NewManagedBundle always returns a recovery-capable registry, including on error.
func NewManagedBundle(root string) *Bundle {
	s := &templateStore{root: root, bundles: map[string]*Bundle{}, inventory: map[string]Version{}}
	b := &Bundle{managed: s}
	embedded, embedErr := newEmbeddedBundle()
	if embedErr == nil {
		b.assetSet, b.htmlPreviewIntegrity = embedded.assetSet, embedded.htmlPreviewIntegrity
	}
	// Historical resource serving does not depend on the embedded current template.
	if files, err := fs.Sub(embeddedFiles, "files"); err == nil {
		b.files = files
		b.fileServer = assetFileServer(files)
		versions, err := (&Bundle{files: files}).Versions()
		if err == nil {
			for _, v := range versions {
				v.Current = false
				s.inventory[v.AssetSet] = v
			}
		}
	}
	if !filepath.IsAbs(root) {
		s.err = ErrUnavailable
		return b
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		s.err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return b
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		s.err = ErrUnavailable
		return b
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		s.err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return b
	}
	if len(entries) == 0 {
		if err = syncDir(filepath.Dir(root)); err != nil {
			s.err = fmt.Errorf("%w: %v", ErrUnavailable, err)
			return b
		}
		if embedErr != nil {
			s.err = embedErr
			return b
		}
		initial, err := s.installEmbedded(embedded)
		if err != nil {
			s.err = err
			return b
		}
		if err = b.Activate(initial); err != nil {
			s.err = err
		}
		return b
	}
	for _, entry := range entries {
		if !assetSetPattern.MatchString(entry.Name()) {
			continue
		}
		loaded, v, loadErr := loadDiskPackage(filepath.Join(root, entry.Name()), entry.Name())
		if loadErr != nil {
			v = Version{AssetSet: entry.Name(), Error: loadErr.Error()}
		} else {
			s.bundles[v.AssetSet] = loaded
		}
		s.inventory[entry.Name()] = v
	}
	raw, err := os.ReadFile(filepath.Join(root, "current"))
	if err != nil {
		s.err = fmt.Errorf("%w: current is missing or unreadable", ErrUnavailable)
		return b
	}
	id := strings.TrimSuffix(string(raw), "\n")
	if !assetSetPattern.MatchString(id) {
		s.err = fmt.Errorf("%w: invalid current", ErrUnavailable)
		return b
	}
	s.current = id
	s.active = s.bundles[id]
	if s.active == nil {
		s.err = fmt.Errorf("%w: selected version is unavailable", ErrUnavailable)
	}
	return b
}
func loadDiskPackage(root, id string) (*Bundle, Version, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, Version{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, Version{}, ErrInvalidPackage
	}
	b, v, err := loadPackage(os.DirFS(root))
	if err == nil && v.AssetSet != id {
		return nil, Version{}, ErrInvalidPackage
	}
	return b, v, err
}
func (s *templateStore) step(name string) error {
	if s.before != nil {
		return s.before(name)
	}
	return nil
}
func syncDir(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *templateStore) syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			dirs = append(dirs, name)
			return nil
		}
		if err = s.step("file-sync"); err != nil {
			return err
		}
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = s.step("directory-sync"); err != nil {
			return err
		}
		if err = syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}
func (s *templateStore) installEmbedded(embedded *Bundle) (string, error) {
	// Freeze the existing embedded renderer into the current package contract.
	// Only package metadata and the three version references change; historical
	// resource URLs and bytes remain embedded and independently available.
	raw, err := embeddedFiles.ReadFile("conversation-assets.json")
	if err != nil {
		return "", err
	}
	var manifest assetManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return "", err
	}
	normalized, err := normalizeVersion(embedded.template, embedded.assetSet)
	if err != nil {
		return "", err
	}
	records := make(map[string]string, len(manifest.Files))
	for _, file := range manifest.Files {
		records[file.Path] = file.SHA256
	}
	id := packageDigest(normalized, records)
	template, err := replaceTemplateVersion(normalized, assetSetMarker, id)
	if err != nil {
		return "", err
	}
	manifest.AssetSet = id
	if manifest.Build == nil {
		manifest.Build = &BuildSource{}
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(s.root, ".upload-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for name, data := range map[string][]byte{"conversation.template.html": template, "conversation-assets.json": raw} {
		if err = os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
			return "", err
		}
	}
	prefix := "files/" + embedded.assetSet
	err = fs.WalkDir(embeddedFiles, prefix, func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(stage, "assets", id, strings.TrimPrefix(name, prefix))
		if e.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := embeddedFiles.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		return "", err
	}
	id, _, err = s.commit(stage)
	return id, err
}
func (s *templateStore) commit(stage string) (string, bool, error) {
	loaded, v, err := loadPackage(os.DirFS(stage))
	if err != nil {
		return "", false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	destination := filepath.Join(s.root, v.AssetSet)
	if _, err = os.Lstat(destination); err == nil {
		existing, first, err := loadDiskPackage(destination, v.AssetSet)
		if err != nil {
			s.mu.Lock()
			s.inventory[v.AssetSet] = Version{AssetSet: v.AssetSet, Error: err.Error()}
			delete(s.bundles, v.AssetSet)
			if s.current == v.AssetSet {
				s.active = nil
				s.err = err
			}
			s.mu.Unlock()
			return "", false, fmt.Errorf("%w: stored version is invalid", ErrConflict)
		}
		if err = s.syncTree(destination); err != nil {
			return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if err = syncDir(s.root); err != nil {
			return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		s.mu.Lock()
		s.bundles[v.AssetSet] = existing
		s.inventory[v.AssetSet] = first
		s.mu.Unlock()
		return v.AssetSet, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = s.syncTree(stage); err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = s.step("package-rename"); err == nil {
		err = os.Rename(stage, destination)
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = s.step("package-parent-sync"); err == nil {
		err = syncDir(s.root)
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: package persistence uncertain: %v", ErrUnavailable, err)
	}
	// The staged FS path changed. Repoint only the immutable resource handler.
	loaded.files = os.DirFS(filepath.Join(destination, "assets"))
	loaded.fileServer = assetFileServer(loaded.files)
	s.mu.Lock()
	s.bundles[v.AssetSet] = loaded
	s.inventory[v.AssetSet] = v
	s.mu.Unlock()
	return v.AssetSet, false, nil
}
func (b *Bundle) Upload(reader io.ReaderAt, size int64) (string, bool, error) {
	if b.managed == nil {
		return "", false, ErrUnavailable
	}
	if size > MaxUploadBytes {
		return "", false, ErrTooLarge
	}
	if size < 0 {
		return "", false, ErrInvalidPackage
	}
	z, err := zip.NewReader(reader, size)
	if err != nil {
		return "", false, ErrInvalidPackage
	}
	if len(z.File) > 4096 {
		return "", false, ErrTooLarge
	}
	s := b.managed
	stage, err := os.MkdirTemp(s.root, ".upload-")
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer os.RemoveAll(stage)
	seen := map[string]bool{}
	var total int64
	for _, f := range z.File {
		name := strings.TrimSuffix(f.Name, "/")
		if !validAssetPath(name) || seen[name] {
			return "", false, ErrInvalidPackage
		}
		seen[name] = true
		target := filepath.Join(stage, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if f.Mode().Type() != os.ModeDir {
				return "", false, ErrInvalidPackage
			}
			if err = os.MkdirAll(target, 0700); err != nil {
				return "", false, ErrInvalidPackage
			}
			continue
		}
		if !f.Mode().IsRegular() || strings.HasSuffix(f.Name, "/") {
			return "", false, ErrInvalidPackage
		}
		limit := maxFileBytes
		if name == "conversation.template.html" {
			limit = 256 << 10
		}
		if name == "conversation-assets.json" {
			limit = 1 << 20
		}
		if filepath.Base(name) == "runtime.js" {
			limit = 2 << 20
		}
		if f.UncompressedSize64 > uint64(limit) {
			return "", false, ErrTooLarge
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", false, ErrInvalidPackage
		}
		src, err := f.Open()
		if err != nil {
			return "", false, ErrInvalidPackage
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return "", false, ErrUnavailable
		}
		if remaining := maxExpandedBytes - total; remaining < limit {
			limit = remaining
		}
		n, copyErr := io.Copy(dst, io.LimitReader(src, limit+1))
		closeErr := dst.Close()
		src.Close()
		total += n
		if n > limit {
			return "", false, ErrTooLarge
		}
		if copyErr != nil {
			return "", false, ErrInvalidPackage
		}
		if closeErr != nil {
			return "", false, ErrUnavailable
		}
	}
	return s.commit(stage)
}
func (b *Bundle) Activate(id string) error {
	if b.managed == nil {
		return ErrUnavailable
	}
	if !assetSetPattern.MatchString(id) {
		return ErrNotFound
	}
	s := b.managed
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	loaded, v, err := loadDiskPackage(filepath.Join(s.root, id), id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		s.mu.Lock()
		s.inventory[id] = Version{AssetSet: id, Error: err.Error()}
		delete(s.bundles, id)
		if s.current == id {
			s.active = nil
			s.err = err
		}
		s.mu.Unlock()
		return fmt.Errorf("%w: target is invalid", ErrConflict)
	}
	temp, err := os.CreateTemp(s.root, ".current-")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer os.Remove(temp.Name())
	if err = s.step("current-write"); err == nil {
		_, err = temp.WriteString(id + "\n")
	}
	if err == nil {
		err = s.step("current-sync")
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = s.step("current-rename")
	}
	if err == nil {
		err = os.Rename(temp.Name(), filepath.Join(s.root, "current"))
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err = s.step("current-parent-sync"); err == nil {
		err = syncDir(s.root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = id
	if err != nil {
		s.active = nil
		s.err = fmt.Errorf("%w: current persistence uncertain", ErrUnavailable)
		return s.err
	}
	s.bundles[id] = loaded
	s.inventory[id] = v
	s.active = loaded
	s.err = nil
	return nil
}
func (s *templateStore) versions() ([]Version, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Version, 0, len(s.inventory))
	for id, v := range s.inventory {
		v.Current = id == s.current
		if v.Current && s.err != nil {
			v.Error = s.err.Error()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].AssetSet < out[j].AssetSet
	})
	return out, s.err
}
func (b *Bundle) UploadEnabled() bool {
	if b.managed == nil {
		return false
	}
	info, err := os.Lstat(b.managed.root)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && syscall.Access(b.managed.root, 2) == nil
}
func (b *Bundle) selected() (*Bundle, error) {
	if b.managed == nil {
		return b, nil
	}
	s := b.managed
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.err != nil || s.active == nil {
		return nil, ErrUnavailable
	}
	return s.active, nil
}

func assetFileServer(files fs.FS) http.Handler {
	return http.StripPrefix(PublicPathPrefix, http.FileServer(http.FS(files)))
}
