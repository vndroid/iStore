package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vndroid/istore/internal/auximageprovider"
	"github.com/vndroid/istore/internal/processing"
	"github.com/vndroid/istore/internal/source"
)

func writeStyles(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "styles.json")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadStyles(t *testing.T) {
	path := writeStyles(t, `{"thumb":"image/resize,w_200/quality,q_80","hero.v2":"image/resize,w_1600/format,auto"}`)
	styles, err := loadStyles(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := expandStyle("style/thumb", styles); err != nil || got != "image/resize,w_200/quality,q_80" {
		t.Fatalf("expandStyle = %q, %v", got, err)
	}
	if got, err := expandStyle("image/resize,w_300", styles); err != nil || got != "image/resize,w_300" {
		t.Fatalf("ordinary chain = %q, %v", got, err)
	}
	for _, raw := range []string{"style/nope", "style/thumb/quality,q_80", "style/", "style/thumb,"} {
		if _, err := expandStyle(raw, styles); err == nil {
			t.Errorf("expandStyle(%q) accepted", raw)
		}
	}
}

func TestStyleStartupValidation(t *testing.T) {
	for _, tc := range []struct{ name, json, want string }{
		{"missing", "", "load styles"},
		{"array", `[]`, "JSON object"},
		{"trailing", `{} {}`, "trailing JSON"},
		{"bad name", `{"a/b":"image/resize,w_200"}`, "name must"},
		{"long name", `{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"image/resize,w_200"}`, "name must"},
		{"recursive", `{"a":"style/b","b":"image/resize,w_200"}`, "recursive"},
		{"empty", `{"a":""}`, "empty processing"},
		{"invalid action", `{"a":"image/not-an-action"}`, "style \"a\""},
		{"invalid argument", `{"a":"image/resize,w_nope"}`, "style \"a\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "styles.json")
			if tc.name != "missing" {
				path = writeStyles(t, tc.json)
			}
			_, err := loadStyles(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadStyles error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestStyleCacheKeyChangesWithDefinition(t *testing.T) {
	info := &source.Info{Key: "/images/a.png", Size: 123, Version: "42"}
	s := &Server{}
	a, err := expandStyle("style/thumb", map[string]string{"thumb": "image/resize,w_200"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := expandStyle("style/thumb", map[string]string{"thumb": "image/resize,w_300"})
	if err != nil {
		t.Fatal(err)
	}
	if s.cacheKey(info, a).Hash() == s.cacheKey(info, b).Hash() {
		t.Fatal("changed style reused the old cache key")
	}
}

func TestStyleRequestKeepsAliasInSignature(t *testing.T) {
	v := &recordingVerifier{enabled: true}
	s, err := New(Config{
		Root:       t.TempDir(),
		StylesFile: writeStyles(t, `{"thumb":"image/resize,w_200"}`),
		Verifier:   v,
	}, func(auximageprovider.Provider) (*processing.Processor, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/missing.jpg?x-oss-process=style/thumb&x-istore-signature=abc", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("style request status = %d, want 404 for missing source: %s", w.Code, w.Body.String())
	}
	if v.gotMessage != "/missing.jpg?x-oss-process=style/thumb" {
		t.Fatalf("signed message = %q", v.gotMessage)
	}

	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/missing.jpg?x-oss-process=style/unknown&x-istore-signature=abc", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown style status = %d, want 400", w.Code)
	}
}
