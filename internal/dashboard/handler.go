package dashboard

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
)

type asset struct {
	contentType string
	body        []byte
}

type Handler struct {
	assets map[string]asset
}

func New(source fs.FS) (*Handler, error) {
	dist, err := fs.Sub(source, "dist")
	if err != nil {
		return nil, err
	}
	definitions := map[string]struct {
		file        string
		contentType string
	}{
		"/":                  {"index.html", "text/html; charset=utf-8"},
		"/index.html":        {"index.html", "text/html; charset=utf-8"},
		"/assets/styles.css": {"assets/styles.css", "text/css; charset=utf-8"},
		"/assets/state.js":   {"assets/state.js", "text/javascript; charset=utf-8"},
		"/assets/app.js":     {"assets/app.js", "text/javascript; charset=utf-8"},
	}
	handler := &Handler{assets: make(map[string]asset, len(definitions))}
	for requestPath, definition := range definitions {
		body, readErr := fs.ReadFile(dist, definition.file)
		if readErr != nil || len(body) == 0 {
			if readErr == nil {
				readErr = errors.New("dashboard asset is empty")
			}
			return nil, readErr
		}
		handler.assets[requestPath] = asset{contentType: definition.contentType, body: body}
	}
	return handler, nil
}

// DashboardHandler marks this handler as the optional UI provider consumed by
// httpapi without coupling the static package back to the API package.
func (handler *Handler) DashboardHandler() http.Handler { return handler }

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(writer.Header())
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method is not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path != path.Clean(request.URL.Path) && request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	item, ok := handler.assets[request.URL.Path]
	if !ok {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", item.contentType)
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(item.body)
	}
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
