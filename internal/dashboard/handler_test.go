package dashboard

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	webassets "github.com/Versifine/study-monitor/web"
)

func TestEmbeddedDashboardServesOnlyWhitelistedReadOnlyAssets(t *testing.T) {
	handler, err := New(webassets.Dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, contentType string
		status                    int
	}{
		{http.MethodGet, "/", "text/html; charset=utf-8", http.StatusOK},
		{http.MethodHead, "/assets/app.js", "text/javascript; charset=utf-8", http.StatusOK},
		{http.MethodGet, "/assets/styles.css", "text/css; charset=utf-8", http.StatusOK},
		{http.MethodGet, "/missing", "", http.StatusNotFound},
		{http.MethodPost, "/", "", http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.contentType != "" && response.Header().Get("Content-Type") != test.contentType {
				t.Fatalf("content type=%q", response.Header().Get("Content-Type"))
			}
			if test.method == http.MethodHead && response.Body.Len() != 0 {
				t.Fatalf("HEAD body=%q", response.Body.String())
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'none'") || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers=%#v", response.Header())
	}
	if !strings.Contains(response.Body.String(), "data-panel=\"storage\"") || strings.Contains(strings.ToLower(response.Body.String()), "<form") {
		t.Fatal("embedded page is missing its storage panel or contains a form")
	}
}

func TestDashboardInitializationFailsClosedForMissingOrEmptyAssets(t *testing.T) {
	missing := fstest.MapFS{"dist/index.html": {Data: []byte("ok")}}
	if _, err := New(missing); err == nil {
		t.Fatal("missing asset initialized successfully")
	}
	empty := fstest.MapFS{
		"dist/index.html":        {Data: []byte("ok")},
		"dist/assets/styles.css": {Data: []byte("ok")},
		"dist/assets/state.js":   {Data: []byte("ok")},
		"dist/assets/app.js":     {Data: nil},
	}
	if _, err := New(empty); err == nil {
		t.Fatal("empty asset initialized successfully")
	}
	if _, err := fs.Sub(missing, "dist"); err != nil {
		t.Fatal(err)
	}
}
