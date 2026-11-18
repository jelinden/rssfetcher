package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The UI has to travel inside the binary: the old package level
// template.Must(template.ParseGlob("public/tmpl/*")) panicked during init
// whenever the process was started from another working directory.
func TestTemplatesAreEmbedded(t *testing.T) {
	tmpl, err := LoadTemplates("")
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	for _, name := range []string{"view", "edit", "login", "header", "footer", "messages", "csrf"} {
		if tmpl.Lookup(name) == nil {
			t.Errorf("embedded template %q is missing", name)
		}
	}
	if len(TemplateNames()) < 4 {
		t.Errorf("TemplateNames() = %v, want the login/view/edit/partials files", TemplateNames())
	}
}

func TestStylesheetIsEmbeddedAndServed(t *testing.T) {
	if !contains(AssetNames(), "style.css") {
		t.Fatalf("AssetNames() = %v, want style.css", AssetNames())
	}
	body, header := getAsset(t, "/static/style.css")
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the served stylesheet is not the expected one")
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestStaticAssetMissingIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/nope.css", nil)
	testHandler(t, newFakeStore()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Every POST form in the UI must carry the CSRF token, otherwise the guard is
// silently bypassed by a template change.
func TestAllPostFormsCarryCSRFToken(t *testing.T) {
	form := regexp.MustCompile(`(?is)<form[^>]*method="POST"[^>]*>(.*?)</form>`)
	for _, name := range TemplateNames() {
		data, err := templateFS.ReadFile("tmpl/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, match := range form.FindAllStringSubmatch(string(data), -1) {
			if !strings.Contains(match[1], "csrf_token") && !strings.Contains(match[1], `template "csrf"`) {
				t.Errorf("%s form #%d posts without a csrf_token field", name, i+1)
			}
		}
	}
}

func TestTemplatesLinkStylesheet(t *testing.T) {
	for _, name := range TemplateNames() {
		data, err := templateFS.ReadFile("tmpl/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "partials.html" && !strings.Contains(string(data), `/static/style.css`) {
			t.Error("the shared header does not link the stylesheet")
		}
	}
}

func TestTemplatesRenderWithoutExternalCSSRequest(t *testing.T) {
	c := authenticatedClient(t, newFakeStore())
	body := c.get("/view/").Body.String()
	if !strings.Contains(body, `<link rel="stylesheet" href="/static/style.css">`) {
		t.Error("rendered page does not reference the embedded stylesheet")
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Error("rendered page is missing the doctype")
	}
}

func TestLoadTemplatesFromDiskOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "view.html"), []byte(`{{define "view"}}disk{{end}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := LoadTemplates(dir)
	if err != nil {
		t.Fatalf("LoadTemplates(dir): %v", err)
	}
	rec := httptest.NewRecorder()
	if err := tmpl.ExecuteTemplate(rec, "view", nil); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "disk" {
		t.Errorf("rendered %q, want the on disk template to win", rec.Body.String())
	}
}

func TestLoadTemplatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := LoadTemplates(dir); err == nil {
		t.Fatal("LoadTemplates accepted a directory that does not exist")
	}
	empty := t.TempDir()
	if _, err := LoadTemplates(empty); err == nil {
		t.Fatal("LoadTemplates accepted a directory without templates")
	}
}

func getAsset(t *testing.T, path string) (string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	StaticHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), rec.Header()
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
