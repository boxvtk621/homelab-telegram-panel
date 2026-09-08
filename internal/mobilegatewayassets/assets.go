// Package mobilegatewayassets serves the reviewed Mobile Workspace bundle
// embedded in the standalone Mobile Gateway binary.
package mobilegatewayassets

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	maximumBundleBytes    = 8 << 20
	maximumAssetBytes     = 4 << 20
)

var (
	//go:embed dist
	embeddedBundle embed.FS

	fingerprintedAsset = regexp.MustCompile(`^assets/[A-Za-z0-9._-]+-[A-Za-z0-9_-]{8}\.(?:css|js)$`)
	resourceAttribute  = regexp.MustCompile(`(?i)\b(?:src|href)\s*=`)
	resourceReference  = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*"([^"]+)"`)
	scriptElement      = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	integrityAttribute = regexp.MustCompile(`(?i)\bintegrity="([^"]+)"`)
)

type asset struct {
	payload     []byte
	contentType string
	immutable   bool
}

type handler struct {
	assets map[string]asset
}

// NewHandler validates the embedded release artifact before returning its
// fail-closed HTTP handler. Invalid or incomplete bundles never reach serve.
func NewHandler() (http.Handler, error) {
	dist, err := fs.Sub(embeddedBundle, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded Mobile Workspace bundle: %w", err)
	}
	return newHandler(dist)
}

func newHandler(dist fs.FS) (http.Handler, error) {
	if dist == nil {
		return nil, errors.New("Mobile Workspace bundle is not configured")
	}
	assets := make(map[string]asset)
	totalBytes := 0
	err := fs.WalkDir(dist, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name != "." && name != "assets" {
				return fmt.Errorf("Mobile Workspace bundle contains unsupported directory %q", name)
			}
			return nil
		}
		if !fs.ValidPath(name) || path.Clean(name) != name || !allowedBundleFile(name) {
			return fmt.Errorf("Mobile Workspace bundle contains unsupported file %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect Mobile Workspace asset %q: %w", name, err)
		}
		if info.Size() < 1 || info.Size() > maximumAssetBytes || info.Mode()&fs.ModeType != 0 {
			return fmt.Errorf("Mobile Workspace asset %q has invalid type or size", name)
		}
		totalBytes += int(info.Size())
		if totalBytes > maximumBundleBytes {
			return errors.New("Mobile Workspace bundle exceeds its byte bound")
		}
		payload, err := fs.ReadFile(dist, name)
		if err != nil {
			return fmt.Errorf("read Mobile Workspace asset %q: %w", name, err)
		}
		assets["/"+name] = asset{
			payload: payload, contentType: contentType(name), immutable: fingerprintedAsset.MatchString(name),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"/index.html", "/favicon.svg", "/og.png"} {
		if _, ok := assets[required]; !ok {
			return nil, fmt.Errorf("Mobile Workspace bundle is missing %q", required)
		}
	}
	if err := validateIndex(assets); err != nil {
		return nil, err
	}
	return &handler{assets: assets}, nil
}

func allowedBundleFile(name string) bool {
	switch name {
	case "index.html", "telegram-web-app.js", "favicon.svg", "og.png":
		return true
	default:
		return fingerprintedAsset.MatchString(name)
	}
}

func validateIndex(assets map[string]asset) error {
	index := string(assets["/index.html"].payload)
	if strings.Contains(strings.ToLower(index), "<base") {
		return errors.New("Mobile Workspace index must not override its base URL")
	}
	references := resourceReference.FindAllStringSubmatch(index, -1)
	if len(references) != len(resourceAttribute.FindAllStringIndex(index, -1)) {
		return errors.New("Mobile Workspace index contains an unsupported resource attribute")
	}
	for _, match := range references {
		reference := match[1]
		if strings.HasPrefix(reference, "./") {
			reference = strings.TrimPrefix(reference, ".")
		}
		if !strings.HasPrefix(reference, "/") || strings.HasPrefix(reference, "//") {
			return fmt.Errorf("Mobile Workspace index contains non-local resource %q", reference)
		}
		if _, ok := assets[reference]; !ok {
			return fmt.Errorf("Mobile Workspace index references missing asset %q", reference)
		}
	}
	scripts := scriptElement.FindAllString(index, -1)
	if len(scripts) < 1 || strings.Count(strings.ToLower(index), "<script") != len(scripts) {
		return errors.New("Mobile Workspace index contains an inline or malformed script")
	}
	moduleFound := false
	for _, script := range scripts {
		if !strings.Contains(script, `src="`) {
			return errors.New("Mobile Workspace index contains an inline script")
		}
		if strings.Contains(script, `src="/telegram-web-app.js"`) {
			integrity := integrityAttribute.FindStringSubmatch(script)
			if len(integrity) != 2 || integrity[1] != sha256Integrity(assets["/telegram-web-app.js"].payload) {
				return errors.New("Mobile Workspace Telegram SDK integrity does not match the embedded asset")
			}
		}
		if strings.Contains(script, `type="module"`) && (strings.Contains(script, `src="/assets/`) || strings.Contains(script, `src="./assets/`)) {
			moduleFound = true
		}
	}
	if !moduleFound {
		return errors.New("Panel index is missing its application module")
	}
	return nil
}

func sha256Integrity(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256-" + base64.StdEncoding.EncodeToString(digest[:])
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		panic("validated Mobile Workspace asset has no content type")
	}
}

func (handler *handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response.Header())
	if handler == nil || request == nil || request.URL == nil ||
		request.URL.RawPath != "" || request.URL.Path == "" ||
		path.Clean(request.URL.Path) != request.URL.Path || strings.Contains(request.URL.Path, "//") ||
		request.URL.RawQuery != "" || request.URL.ForceQuery ||
		request.ContentLength != 0 || len(request.TransferEncoding) != 0 ||
		(request.Body != nil && request.Body != http.NoBody) {
		notFound(response)
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	requestPath := request.URL.Path
	if requestPath == "/" {
		requestPath = "/index.html"
	}
	selected, ok := handler.assets[requestPath]
	if !ok {
		notFound(response)
		return
	}
	if requestPath == "/index.html" {
		response.Header().Set("Cache-Control", "no-store")
	} else if selected.immutable {
		response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		response.Header().Set("Cache-Control", "no-cache")
	}
	response.Header().Set("Content-Type", selected.contentType)
	response.Header().Set("Content-Length", strconv.Itoa(len(selected.payload)))
	response.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = response.Write(selected.payload)
	}
}

func setSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

func notFound(response http.ResponseWriter) {
	response.Header().Set("Cache-Control", "no-store")
	http.Error(response, "not found", http.StatusNotFound)
}
