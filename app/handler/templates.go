package handler

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jelinden/rssfetcher/app/domain"
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

// templateFuncs are the helpers the markup uses; they are registered before
// parsing because a template that names an unknown function fails to parse.
var templateFuncs = template.FuncMap{
	// dueIn explains why a feed has not been fetched again: "" when it is due now,
	// otherwise "next poll in 42m".
	"dueIn": func(feed domain.Feed, now time.Time, maxTTL time.Duration) string {
		wait := feed.DueIn(now, maxTTL)
		if wait <= 0 {
			return ""
		}
		return "next poll in " + humanDuration(wait)
	},
}

// humanDuration rounds a wait to something worth reading in a table: 45s, 42m,
// 1h30m. time.Duration.String() would print "59m0s" for every minute-sized wait,
// which is noise in a list of feeds.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		// Rounded up: "next poll in 0s" for a feed that is due in a moment would
		// read as though the fetcher had stopped.
		return fmt.Sprintf("%ds", int(math.Ceil(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(math.Ceil(d.Minutes())))
	}
	total := int(math.Ceil(d.Minutes()))
	h, m := total/60, total%60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// LoadTemplates parses the UI templates. An empty dir uses the embedded copies,
// which is what a deployed binary needs; passing a directory reads them from
// disk instead so the markup can be tweaked without a rebuild.
func LoadTemplates(dir string) (*template.Template, error) {
	if dir == "" {
		tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "tmpl/*.html")
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
	tmpl, err := template.New("").Funcs(templateFuncs).ParseGlob(pattern)
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
