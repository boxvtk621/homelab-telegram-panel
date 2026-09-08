package mobilegatewayassets

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func TestEmbeddedHandlerServesOnlyReviewedBundle(t *testing.T) {
	t.Parallel()

	served, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	bundle := served.(*handler)
	if len(bundle.assets) < 6 {
		t.Fatalf("embedded asset count = %d, want at least 6", len(bundle.assets))
	}
	for requestPath, expected := range bundle.assets {
		requestPath := requestPath
		expected := expected
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			method := method
			t.Run(method+" "+requestPath, func(t *testing.T) {
				t.Parallel()
				response := serve(t, served, method, requestPath)
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d; body: %s", response.Code, response.Body.String())
				}
				assertSecurityHeaders(t, response)
				if got := response.Header().Get("Content-Type"); got != expected.contentType {
					t.Fatalf("Content-Type = %q, want %q", got, expected.contentType)
				}
				if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(expected.payload)) {
					t.Fatalf("Content-Length = %q", got)
				}
				wantCache := "no-cache"
				if requestPath == "/index.html" {
					wantCache = "no-store"
				} else if expected.immutable {
					wantCache = "public, max-age=31536000, immutable"
				}
				if got := response.Header().Get("Cache-Control"); got != wantCache {
					t.Fatalf("Cache-Control = %q, want %q", got, wantCache)
				}
				if method == http.MethodHead {
					if response.Body.Len() != 0 {
						t.Fatalf("HEAD body length = %d", response.Body.Len())
					}
				} else if !bytes.Equal(response.Body.Bytes(), expected.payload) {
					t.Fatal("GET payload differs from embedded asset")
				}
			})
		}
	}

	root := serve(t, served, http.MethodGet, "/")
	index := serve(t, served, http.MethodGet, "/index.html")
	if root.Code != http.StatusOK || !bytes.Equal(root.Body.Bytes(), index.Body.Bytes()) {
		t.Fatal("root route does not serve the exact no-store index")
	}
}

func TestHandlerRejectsNonCanonicalOrAmbiguousRequests(t *testing.T) {
	t.Parallel()

	served, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantStatus int
	}{
		{name: "post", mutate: func(request *http.Request) { request.Method = http.MethodPost }, wantStatus: http.StatusMethodNotAllowed},
		{name: "options", mutate: func(request *http.Request) { request.Method = http.MethodOptions }, wantStatus: http.StatusMethodNotAllowed},
		{name: "query", mutate: func(request *http.Request) { request.URL.RawQuery = "v=1" }, wantStatus: http.StatusNotFound},
		{name: "empty query marker", mutate: func(request *http.Request) { request.URL.ForceQuery = true }, wantStatus: http.StatusNotFound},
		{name: "raw path", mutate: func(request *http.Request) { request.URL.RawPath = "/%69ndex.html" }, wantStatus: http.StatusNotFound},
		{name: "empty path", mutate: func(request *http.Request) { request.URL.Path = "" }, wantStatus: http.StatusNotFound},
		{name: "double slash", mutate: func(request *http.Request) { request.URL.Path = "//index.html" }, wantStatus: http.StatusNotFound},
		{name: "dot segment", mutate: func(request *http.Request) { request.URL.Path = "/assets/../index.html" }, wantStatus: http.StatusNotFound},
		{name: "body", mutate: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader("hostile"))
			request.ContentLength = 7
		}, wantStatus: http.StatusNotFound},
		{name: "body with hidden length", mutate: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader("hostile"))
			request.ContentLength = 0
		}, wantStatus: http.StatusNotFound},
		{name: "chunked body", mutate: func(request *http.Request) { request.TransferEncoding = []string{"chunked"} }, wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "https://mobile.example.test/index.html", nil)
			test.mutate(request)
			response := httptest.NewRecorder()
			served.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			assertSecurityHeaders(t, response)
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("rejection cache/CORS headers = %q/%q", response.Header().Get("Cache-Control"), response.Header().Get("Access-Control-Allow-Origin"))
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow = %q", response.Header().Get("Allow"))
			}
		})
	}
}

func TestHandlerNeverFallsBackOrListsDirectories(t *testing.T) {
	t.Parallel()

	served, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	for _, requestPath := range []string{
		"/api", "/api/", "/api/v1/unknown", "/assets", "/assets/", "/missing", "/favicon.svg/", "/index.html/",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			response := serve(t, served, method, requestPath)
			if response.Code != http.StatusNotFound {
				t.Errorf("%s %s status = %d, want 404", method, requestPath, response.Code)
			}
			assertSecurityHeaders(t, response)
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s is cacheable", method, requestPath)
			}
		}
	}
}

func TestEmbeddedBundleHasNoSourceMapsOrRemoteEntrypoints(t *testing.T) {
	t.Parallel()

	dist, err := fs.Sub(embeddedBundle, "dist")
	if err != nil {
		t.Fatalf("fs.Sub(): %v", err)
	}
	if err := fs.WalkDir(dist, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(name), ".map") {
			t.Errorf("embedded source map: %s", name)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk embedded bundle: %v", err)
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	for _, match := range resourceReference.FindAllStringSubmatch(string(index), -1) {
		if !strings.HasPrefix(match[1], "/") || strings.HasPrefix(match[1], "//") {
			t.Errorf("non-local runtime resource: %q", match[1])
		}
	}
}

func TestHandlerIsRaceSafe(t *testing.T) {
	t.Parallel()

	served, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler(): %v", err)
	}
	paths := []string{"/", "/index.html", "/telegram-web-app.js", "/favicon.svg", "/og.png", "/missing"}
	var group sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		for _, requestPath := range paths {
			group.Add(1)
			go func(requestPath string) {
				defer group.Done()
				response := serve(t, served, http.MethodGet, requestPath)
				if requestPath == "/missing" && response.Code != http.StatusNotFound {
					t.Errorf("missing status = %d", response.Code)
				} else if requestPath != "/missing" && response.Code != http.StatusOK {
					t.Errorf("%s status = %d", requestPath, response.Code)
				}
			}(requestPath)
		}
	}
	group.Wait()
}

func TestBundleValidationFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(fstest.MapFS)
	}{
		{name: "missing index", mutate: func(bundle fstest.MapFS) { delete(bundle, "index.html") }},
		{name: "empty asset", mutate: func(bundle fstest.MapFS) { bundle["favicon.svg"] = &fstest.MapFile{} }},
		{name: "source map", mutate: func(bundle fstest.MapFS) {
			bundle["assets/index-AbCd1234.js.map"] = &fstest.MapFile{Data: []byte("map")}
		}},
		{name: "unfingerprinted app", mutate: func(bundle fstest.MapFS) { bundle["assets/index.js"] = &fstest.MapFile{Data: []byte("app")} }},
		{name: "nested directory", mutate: func(bundle fstest.MapFS) { bundle["nested/file.js"] = &fstest.MapFile{Data: []byte("app")} }},
		{name: "wrong sdk integrity", mutate: func(bundle fstest.MapFS) { bundle["telegram-web-app.js"] = &fstest.MapFile{Data: []byte("changed")} }},
		{name: "remote script", mutate: func(bundle fstest.MapFS) {
			bundle["index.html"] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(bundle["index.html"].Data), "/assets/index-AbCd1234.js", "https://evil.invalid/app.js"))}
		}},
		{name: "single quoted resource", mutate: func(bundle fstest.MapFS) {
			bundle["index.html"] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(bundle["index.html"].Data), `src="/assets/index-AbCd1234.js"`, `src='/assets/index-AbCd1234.js'`))}
		}},
		{name: "inline script", mutate: func(bundle fstest.MapFS) {
			bundle["index.html"] = &fstest.MapFile{Data: append(bundle["index.html"].Data, []byte("<script>alert(1)</script>")...)}
		}},
		{name: "base override", mutate: func(bundle fstest.MapFS) {
			bundle["index.html"] = &fstest.MapFile{Data: append([]byte(`<base href="https://evil.invalid/">`), bundle["index.html"].Data...)}
		}},
		{name: "missing referenced asset", mutate: func(bundle fstest.MapFS) { delete(bundle, "assets/index-AbCd1234.js") }},
		{name: "oversized asset", mutate: func(bundle fstest.MapFS) { bundle["og.png"] = &fstest.MapFile{Data: make([]byte, maximumAssetBytes+1)} }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			bundle := validFixtureBundle()
			test.mutate(bundle)
			if _, err := newHandler(bundle); err == nil {
				t.Fatal("newHandler() succeeded for an invalid bundle")
			}
		})
	}
}

func validFixtureBundle() fstest.MapFS {
	sdk := []byte("local telegram sdk")
	index := fmt.Sprintf(`<!doctype html><html><head><link rel="icon" href="/favicon.svg"><script src="/telegram-web-app.js" integrity="%s"></script><script type="module" src="/assets/index-AbCd1234.js"></script><link rel="stylesheet" href="/assets/index-EfGh5678.css"></head><body></body></html>`, sha256Integrity(sdk))
	return fstest.MapFS{
		"index.html":                &fstest.MapFile{Data: []byte(index)},
		"telegram-web-app.js":       &fstest.MapFile{Data: sdk},
		"favicon.svg":               &fstest.MapFile{Data: []byte("<svg></svg>")},
		"og.png":                    &fstest.MapFile{Data: []byte("png")},
		"assets/index-AbCd1234.js":  &fstest.MapFile{Data: []byte("app")},
		"assets/index-EfGh5678.css": &fstest.MapFile{Data: []byte("css")},
	}
}

func serve(t *testing.T, served http.Handler, method, requestPath string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "https://mobile.example.test"+requestPath, nil)
	response := httptest.NewRecorder()
	served.ServeHTTP(response, request)
	return response
}

func assertSecurityHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if got := response.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	if !strings.Contains(contentSecurityPolicy, "frame-ancestors 'self' https://web.telegram.org") || strings.Contains(contentSecurityPolicy, "*") {
		t.Fatal("frame-ancestors is not bounded to the reviewed Telegram Web origin")
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers are incomplete: %#v", response.Header())
	}
}
