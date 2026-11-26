package shareassets

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const MaxUploadBytes int64 = 64 << 20
const maxExpandedBytes int64 = 128 << 20
const maxFileBytes int64 = 32 << 20
const assetSetMarker = "__CONVERSATION_EXPORT_ASSET_SET__"

var assetSetPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var ErrInvalidPackage = errors.New("invalid template package")
var ErrTooLarge = errors.New("template package exceeds size limit")
var ErrConflict = errors.New("template version conflicts with stored content")
var ErrUnavailable = errors.New("template storage is unavailable")
var ErrNotFound = errors.New("template version not found")

type BuildSource struct {
	Commit *string `json:"commit"`
	Dirty  *bool   `json:"dirty"`
}
type assetFile struct {
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
	Integrity   string `json:"integrity"`
}

func validAssetPath(name string) bool {
	if !relativeAssetPath.MatchString(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
func digestHex(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func fileIntegrity(data []byte) string {
	sum := sha512.Sum384(data)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}
func normalizeVersion(html []byte, version string) ([]byte, error) {
	return replaceTemplateVersion(html, version, assetSetMarker)
}

func replaceTemplateVersion(html []byte, from, to string) ([]byte, error) {
	patterns := []string{
		`(<meta\s+name="conversation-export-asset-set"\s+content=")([^"]+)("\s*/?>)`,
		`(<link\b[^>]*\bhref="__CONVERSATION_EXPORT_ASSET_ORIGIN__/assets/conversation-export/)([^/"\s]+)(/runtime\.css")`,
		`(<script\b[^>]*\bsrc="__CONVERSATION_EXPORT_ASSET_ORIGIN__/assets/conversation-export/)([^/"\s]+)(/runtime\.js")`,
	}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllSubmatchIndex(html, -1)
		if len(matches) != 1 {
			return nil, errors.New("invalid template version positions")
		}
		m := matches[0]
		if string(html[m[4]:m[5]]) != from {
			return nil, errors.New("template version mismatch")
		}
		html = append(append(append([]byte{}, html[:m[4]]...), to...), html[m[5]:]...)
	}
	return html, nil
}
func packageDigest(html []byte, records map[string]string) string {
	names := make([]string, 0, len(records)+1)
	all := make(map[string]string, len(records)+1)
	all["conversation.template.html"] = digestHex(html)
	for name, digest := range records {
		all["assets/"+name] = digest
	}
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	var input strings.Builder
	input.WriteString("conversation-export-package\x00")
	for _, name := range names {
		fmt.Fprintf(&input, "%s\x00%s\n", name, all[name])
	}
	return digestHex([]byte(input.String()))
}
func validateTemplate(html []byte, m assetManifest) error {
	for marker, count := range map[string]int{snapshotMarker: 1, localBrandIDMarker: 1, styleNonceMarker: 2, publicBrandMeta: 1, canReshareMeta: 1, `id="material-icon-sprite"`: 1, `name="conversation-export-profile" content="conversation-snapshot-json-v2"`: 1} {
		if bytes.Count(html, []byte(marker)) != count {
			return errors.New("invalid template placeholder")
		}
	}
	if !bytes.Contains(html, []byte(`name="conversation-export-style-nonce" content="`+styleNonceMarker+`"`)) ||
		!bytes.Contains(html, []byte("style-src-elem "+assetOriginMarker+" 'nonce-"+styleNonceMarker+"'")) ||
		regexp.MustCompile(`(?i)<style\b|\son[a-z]+\s*=|script-src[^;]*'unsafe-inline'|style-src-elem[^;]*'unsafe-inline'`).Match(html) {
		return errors.New("invalid template CSP or inline content")
	}
	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllSubmatch(html, -1)
	if len(scripts) != 2 {
		return errors.New("invalid template scripts")
	}
	snapshot, runtime := false, false
	for _, script := range scripts {
		attrs, body := string(script[1]), strings.TrimSpace(string(script[2]))
		if strings.Contains(attrs, `id="conversation-snapshot"`) && strings.Contains(attrs, `type="application/json"`) && !strings.Contains(attrs, "src=") && body == snapshotMarker {
			snapshot = true
		} else if strings.Contains(attrs, `id="conversation-export-runtime"`) && strings.Contains(attrs, "defer") && body == "" {
			runtime = true
		} else {
			return errors.New("invalid script payload")
		}
	}
	if !snapshot || !runtime {
		return errors.New("missing template scripts")
	}
	tags := regexp.MustCompile(`(?is)<(?:script|link)\b[^>]+(?:src|href)="([^"]+)"[^>]*>`).FindAllSubmatch(html, -1)
	if len(tags) != 2 {
		return errors.New("invalid external resources")
	}
	for _, tag := range tags {
		matched := false
		for _, f := range m.Files {
			if (f.Path == "runtime.js" || f.Path == "runtime.css") && string(tag[1]) == assetOriginMarker+PublicPathPrefix+m.AssetSet+"/"+f.Path && bytes.Contains(tag[0], []byte(`integrity="`+f.Integrity+`"`)) && bytes.Contains(tag[0], []byte(`crossorigin="anonymous"`)) {
				matched = true
			}
		}
		if !matched {
			return errors.New("template SRI or URL mismatch")
		}
	}
	return nil
}

// Uploaded packages are validated independently from the embedded loader.
func loadPackage(source fs.FS) (*Bundle, Version, error) {
	fail := func(err error) (*Bundle, Version, error) {
		return nil, Version{}, fmt.Errorf("%w: %v", ErrInvalidPackage, err)
	}
	expected := map[string]bool{"conversation.template.html": true, "conversation-assets.json": true}
	var total int64
	count := 0
	err := fs.WalkDir(source, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		count++
		if count > 4096 || !validAssetPath(name) {
			return ErrInvalidPackage
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrInvalidPackage
		}
		limit := maxFileBytes
		if name == "conversation.template.html" {
			limit = 256 << 10
		}
		if name == "conversation-assets.json" {
			limit = 1 << 20
		}
		if path.Base(name) == "runtime.js" {
			limit = 2 << 20
		}
		total += info.Size()
		if info.Size() > limit || total > maxExpandedBytes {
			return ErrTooLarge
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	raw, err := fs.ReadFile(source, "conversation-assets.json")
	if err != nil {
		return fail(err)
	}
	var m assetManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return fail(err)
	}
	if m.Profile != "conversation-export-assets" || !assetSetPattern.MatchString(m.AssetSet) || m.Build == nil || len(m.Files) == 0 {
		return fail(errors.New("invalid manifest"))
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var buildFields map[string]json.RawMessage
	_ = json.Unmarshal(fields["build"], &buildFields)
	if _, ok := buildFields["commit"]; !ok {
		return fail(errors.New("missing commit"))
	}
	if _, ok := buildFields["dirty"]; !ok {
		return fail(errors.New("missing dirty"))
	}
	if m.Build.Commit != nil && !regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`).MatchString(*m.Build.Commit) {
		return fail(errors.New("invalid commit"))
	}
	html, err := fs.ReadFile(source, "conversation.template.html")
	if err != nil {
		return fail(err)
	}
	if !utf8.Valid(html) {
		return fail(errors.New("template must be UTF-8"))
	}
	normalized, err := normalizeVersion(html, m.AssetSet)
	if err != nil {
		return fail(err)
	}
	records := map[string]string{}
	var size int64
	bootstrap := ""
	for _, f := range m.Files {
		if !validAssetPath(f.Path) || f.Path == "build-info.json" {
			return fail(errors.New("invalid resource path"))
		}
		name := "assets/" + m.AssetSet + "/" + f.Path
		if expected[name] {
			return fail(errors.New("duplicate resource"))
		}
		expected[name] = true
		data, err := fs.ReadFile(source, name)
		if err != nil {
			return fail(err)
		}
		if int64(len(data)) != f.Bytes || digestHex(data) != f.SHA256 || fileIntegrity(data) != f.Integrity || assetContentType(f.Path) != f.ContentType {
			return fail(errors.New("resource metadata mismatch"))
		}
		records[f.Path] = f.SHA256
		size += f.Bytes
		if f.Path == "html-preview-bootstrap.js" {
			bootstrap = f.Integrity
		}
	}
	if records["runtime.js"] == "" || records["runtime.css"] == "" || bootstrap == "" {
		return fail(errors.New("required resource missing"))
	}
	err = fs.WalkDir(source, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if e.IsDir() {
			for file := range expected {
				if strings.HasPrefix(file, name+"/") {
					return nil
				}
			}
			return ErrInvalidPackage
		}
		if !expected[name] {
			return ErrInvalidPackage
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if packageDigest(normalized, records) != m.AssetSet {
		return fail(errors.New("content hash mismatch"))
	}
	if err = validateTemplate(html, m); err != nil {
		return fail(err)
	}
	files, err := fs.Sub(source, "assets")
	if err != nil {
		return fail(err)
	}
	assetPaths := make(map[string]bool, len(m.Files))
	for _, file := range m.Files {
		assetPaths[file.Path] = true
	}
	b := &Bundle{assetPaths: assetPaths, assetSet: m.AssetSet, template: html, files: files, fileServer: http.StripPrefix(PublicPathPrefix, http.FileServer(http.FS(files))), htmlPreviewIntegrity: bootstrap}
	return b, Version{AssetSet: m.AssetSet, CanActivate: true, Build: m.Build, FileCount: len(m.Files), SizeBytes: size}, nil
}
