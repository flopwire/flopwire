package webapp

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var assets embed.FS

func Handler() http.Handler {
	root, _ := fs.Sub(assets, "dist")
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "." {
			clean = "index.html"
		}
		if _, err := fs.Stat(root, clean); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}
