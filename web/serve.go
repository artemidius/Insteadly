// Package web serves the built single-page app from the Go binary. The production bundle
// is embedded only under the webembed build tag, so the default (`go build ./...`,
// `make verify`) compiles and serves a placeholder without Node installed.
package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler returns the SPA handler: the embedded bundle with webembed, otherwise a
// placeholder page explaining how to build the frontend.
func Handler() http.Handler {
	if handler := distHandler(); handler != nil {
		return handler
	}
	return placeholderHandler()
}

// spaHandler serves an existing file, or index.html for client-side routes. API and
// webhook routes are registered on the same mux before this handler, so they always win.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/")
		if name == "" || !fileExists(fsys, name) {
			serveIndex(w, fsys)
			return
		}
		if strings.HasSuffix(name, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func fileExists(fsys fs.FS, name string) bool {
	if name == "" || strings.Contains(name, "..") {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "web app not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// placeholderHandler is what an untagged build serves at "/". It is deliberately a 200
// with an explanation, so the C5 no-env smoke can boot the binary without npm.
func placeholderHandler() http.Handler {
	const page = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Insteadly — web app not built</title>
<style>body{font-family:system-ui,sans-serif;max-width:42rem;margin:4rem auto;padding:0 1rem;line-height:1.6;color:#1f2933}code{background:#eef2f7;padding:.15rem .35rem;border-radius:.25rem}</style>
</head>
<body>
<h1>Insteadly web app not built</h1>
<p>The Go backend is running, but this binary was compiled without the React bundle.</p>
<p>Build the frontend and embed it:</p>
<pre><code>make web-build
make build</code></pre>
<p>Or run the Vite dev server against this API:</p>
<pre><code>npm run dev</code></pre>
<p>The JSON API remains available under <code>/api/v1</code>; check <code>/healthz</code>.</p>
</body>
</html>
`
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(page))
	})
}
