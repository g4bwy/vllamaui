package core

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"llama-webui/server/internal/chat"
)

// contentTypes is the map the Node adapter carries. mime.TypeByExtension is not
// enough: it depends on the host /etc/mime.types.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".webp":        "image/webp",
	".map":         "application/json",
}

const (
	immutableCache = "public, max-age=31536000, immutable"
	noCache        = "no-cache"
)

// serveStatic hands out the built UI. Only route paths fall back to the SPA.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, urlPath string) {
	rel, err := url.PathUnescape(urlPath)
	if err != nil {
		chat.WriteError(w, http.StatusBadRequest, "bad request path")
		return
	}
	if strings.HasSuffix(rel, "/") {
		rel += "index.html"
	}

	root, err := filepath.Abs(s.d.Cfg.Dist)
	if err != nil {
		chat.WriteError(w, http.StatusInternalServerError, "bad dist path")
		return
	}
	file := filepath.Join(root, filepath.FromSlash("/"+rel))
	// A plain prefix test would accept a sibling such as ../dist-secret.
	if file != root && !strings.HasPrefix(file, root+string(os.PathSeparator)) {
		chat.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}

	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		// A missing .js must 404, or the browser gets HTML where it expects a
		// module and reports a syntax error.
		if hasExtension(file) {
			chat.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		file = filepath.Join(root, "index.html")
		info, err = os.Stat(file)
		if err != nil {
			chat.WriteError(w, http.StatusNotFound, "ui not built: run ./build.sh")
			return
		}
		s.streamFile(w, r, file, info.Size(), "text/html; charset=utf-8", noCache)
		return
	}

	ext := path.Ext(file)
	ctype, ok := contentTypes[ext]
	if !ok {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	cache := noCache
	if strings.Contains(file, string(os.PathSeparator)+"immutable"+string(os.PathSeparator)) {
		cache = immutableCache
	}
	s.streamFile(w, r, file, info.Size(), ctype, cache)
}

// hasExtension reads a file name the way path.extname does in the Node version:
// a leading dot on a bare name is not an extension, so ".env" is treated as a
// route path and falls back to the page rather than answering 404.
func hasExtension(file string) bool {
	return strings.LastIndex(path.Base(file), ".") > 0
}

// streamFile writes one asset. A read error can only land after the status line
// went out, so it is logged and dropped rather than allowed to fail the process.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request, file string, size int64, ctype, cache string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", cache)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		s.d.Log("asset open failed: %s %s", file, err)
		return
	}
	defer f.Close()
	// io.Copy keeps the kernel sendfile path for a plain file.
	if _, err := io.Copy(w, f); err != nil {
		s.d.Log("asset read failed: %s %s", file, err)
	}
}
