package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTest(t *testing.T) *Server {
	return New(Options{Port: 8799, Token: "secret-token-123456", DataDir: t.TempDir(), Version: "test"})
}

func do(s *Server, method, target, host, cookie, ctype, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = host
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "drl", Value: cookie})
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestSecurity(t *testing.T) {
	s := newTest(t)
	const host, tok = "127.0.0.1:8799", "secret-token-123456"
	cases := []struct {
		name                 string
		method, target, host string
		cookie, ctype, body  string
		want                 int
	}{
		{"ping needs no token", "GET", "/api/ping", host, "", "", "", 200},
		{"no token", "GET", "/api/status", host, "", "", "", 401},
		{"wrong token", "GET", "/api/status", host, "nope", "", "", 401},
		{"DNS rebinding host", "GET", "/api/status", "evil.example:8799", tok, "", "", 403},
		{"form post", "POST", "/api/quit", host, tok, "application/x-www-form-urlencoded", "a=1", 415},
		{"ok", "GET", "/api/status", host, tok, "", "", 200},
		{"localhost alias", "GET", "/api/status", "localhost:8799", tok, "", "", 200},
	}
	for _, c := range cases {
		if w := do(s, c.method, c.target, c.host, c.cookie, c.ctype, c.body); w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
	// The token in the URL sets an HttpOnly cookie and redirects.
	w := do(s, "GET", "/?t="+tok, host, "", "", "")
	if w.Code != 302 || !strings.Contains(w.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Errorf("token login: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
}

func TestDriveNeedsAdminWhenNotRoot(t *testing.T) {
	s := newTest(t)
	if _, err := s.sourcePath(""); err == nil {
		t.Error("empty source accepted")
	}
	if _, err := s.sourcePath("/definitely/not/here.img"); err == nil {
		t.Error("missing file accepted")
	}
}
