package main

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/dist
var adminUIAssets embed.FS

func serveAdminUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	index, err := adminUIAssets.ReadFile("ui/dist/index.html")
	if err != nil {
		http.Error(w, "admin UI is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}

func adminUIAssetsHandler() (http.Handler, error) {
	assets, err := fs.Sub(adminUIAssets, "ui/dist/assets")
	if err != nil {
		return nil, err
	}
	return http.StripPrefix("/assets/", http.FileServer(http.FS(assets))), nil
}
