package api

// The admin portal's static files (D4: REDLITE_PORTAL_DIR, the built portal/dist) on the admin port
// under /portal/. A path that names a file is that file, with the content type from its extension;
// any other path without an extension is the single-page app's index.html (the browser router owns
// it); a missing file with an extension (a stale asset) is 404. Directories are never listed.

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// PortalPrefix is where the portal is mounted (the Vite build's base, portal/vite.config.ts).
const PortalPrefix = "/portal/"

// PortalHandler serves dir under PortalPrefix. It fails when dir has no index.html, so a wrong
// REDLITE_PORTAL_DIR stops the instance at boot instead of serving 404s.
func PortalHandler(dir string) (http.Handler, error) {
	root := os.DirFS(dir)
	if st, err := fs.Stat(root, "index.html"); err != nil || st.IsDir() {
		return nil, fmt.Errorf("api: portal dir %q has no index.html", dir)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, PortalPrefix)), "/")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		if name != "" && fs.ValidPath(name) {
			st, err := fs.Stat(root, name)
			switch {
			case err == nil && !st.IsDir():
				if strings.HasPrefix(name, "assets/") { // content-hashed by Vite: safe to cache for good
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, root, name)
				return
			case err != nil && !errors.Is(err, fs.ErrNotExist):
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			case path.Ext(name) != "":
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache") // the app shell: always revalidated, so a deploy is picked up
		http.ServeFileFS(w, r, root, "index.html")
	}), nil
}

// RegisterPortal mounts h on mux: GET (and HEAD) /portal/... and a redirect from /portal.
func RegisterPortal(mux interface {
	Handle(pattern string, h http.Handler)
}, h http.Handler) {
	mux.Handle("GET "+PortalPrefix, h)
	mux.Handle("GET /portal", http.RedirectHandler(PortalPrefix, http.StatusMovedPermanently))
}
