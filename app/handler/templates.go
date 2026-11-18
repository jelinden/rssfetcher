package handler

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The UI is compiled into the binary: parsing "public/tmpl/*" in a package level
// var made the server panic at init whenever it was started from a different
// working directory (and made the package impossible to test).
//
//go:embed tmpl/*.html
var templateFS embed.FS

//go:embed static/*.css
var staticFS embed.FS

// StaticHandler serves the embedded assets. The cache lifetime is short because
// the CSS has no content hash in its name.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS paths are compile time constants; this cannot happen.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// http.FileServer answers a directory with a listing, which would leak
		// the asset names and render an HTML page where none is wanted. Only
		// real files are served.
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if name == "" || strings.HasSuffix(name, "/") {
			http.NotFound(w, r)
			return
		}
		// The filenames carry no content hash, so a long max-age would hide a
		// deploy for its whole window; let the browser revalidate instead.
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")
		// The embed root is "static/", so the URL prefix has to come off before
		// the file server resolves the name.
		http.StripPrefix("/static/", fileServer).ServeHTTP(w, r)
	})
}

// AssetNames lists the embedded stylesheets (used by tests).
func AssetNames() []string { return embeddedNames(staticFS, "static") }

// TemplateNames lists the embedded template files.
func TemplateNames() []string { return embeddedNames(templateFS, "tmpl") }

func embeddedNames(fsys embed.FS, dir string) []string {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, filepath.Base(e.Name()))
		}
	}
	return names
}

// LoadTemplates parses the UI templates. An empty dir uses the embedded copies,
// which is what a deployed binary needs; passing a directory reads them from
// disk instead so the markup can be tweaked without a rebuild.
func LoadTemplates(dir string) (*template.Template, error) {
	if dir == "" {
		tmpl, err := template.ParseFS(templateFS, "tmpl/*.html")
		if err != nil {
			return nil, fmt.Errorf("handler: parsing embedded templates: %w", err)
		}
		return tmpl, nil
	}
	if !dirExists(dir) {
		return nil, fmt.Errorf("handler: template directory %q does not exist", dir)
	}
	pattern := filepath.Join(dir, "*.html")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("handler: globbing %s: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("handler: no templates found in %s", dir)
	}
	tmpl, err := template.ParseGlob(pattern)
	if err != nil {
		return nil, fmt.Errorf("handler: parse templates in %s: %w", dir, err)
	}
	log.Printf("handler: loaded %d templates from %s (embedded copies ignored)", len(matches), dir)
	return tmpl, nil
}

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}
