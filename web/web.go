// Package web embeds the browser device simulator and admin console.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Register serves the static UI.
func Register(mux *http.ServeMux) {
	static, _ := fs.Sub(files, "static")
	fileServer := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("GET /device", serveFile(static, "device.html"))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/device", http.StatusFound)
	})
}

func serveFile(fsys fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, fsys, name)
	}
}
