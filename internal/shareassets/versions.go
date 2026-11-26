package shareassets

import (
	"context"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
)

// Version describes the assets embedded in this running build, not a deployment
// history or a binding from a share to a renderer.
type Version struct {
	CanActivate bool         `json:"canActivate"`
	Build       *BuildSource `json:"build,omitempty"`
	Error       string       `json:"error,omitempty"`
	AssetSet    string       `json:"assetSet"`
	Current     bool         `json:"current"`
	FileCount   int          `json:"fileCount"`
	SizeBytes   int64        `json:"sizeBytes"`
}

func (b *Bundle) CurrentAssetSet() string {
	if b.managed != nil {
		b.managed.mu.RLock()
		defer b.managed.mu.RUnlock()
		return b.managed.current
	}
	return b.assetSet
}

func (b *Bundle) Versions() ([]Version, error) {
	if b.managed != nil {
		return b.managed.versions()
	}
	entries, err := fs.ReadDir(b.files, ".")
	if err != nil {
		return nil, err
	}
	versions := make([]Version, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(entry.Name()) {
			continue
		}
		version := Version{AssetSet: entry.Name(), Current: entry.Name() == b.assetSet}
		err := fs.WalkDir(b.files, entry.Name(), func(name string, item fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if item.IsDir() {
				return nil
			}
			info, err := item.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() && relativeAssetPath.MatchString(path.Clean(name)) {
				version.FileCount++
				version.SizeBytes += info.Size()
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Current != versions[j].Current {
			return versions[i].Current
		}
		return versions[i].AssetSet < versions[j].AssetSet
	})
	return versions, nil
}

// SetResourceObserver must be called before serving requests. The observer is
// best-effort telemetry and must not change the public response.
func (b *Bundle) SetResourceObserver(observer func(context.Context, string)) {
	b.observeResource = observer
}

type assetResponseWriter struct {
	http.ResponseWriter
	status int
	failed bool
}

func (w *assetResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *assetResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *assetResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.failed = w.failed || err != nil || n != len(body)
	return n, err
}
