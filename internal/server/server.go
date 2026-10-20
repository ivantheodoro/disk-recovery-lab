// Package server is the local HTTP API and embedded web UI. It listens on
// 127.0.0.1 only and requires a per-launch secret token (sent once in the
// URL, then kept in an HttpOnly cookie). Because the server may run with
// administrator rights, requests whose Host header is not this server are
// refused, which blocks DNS-rebinding attacks from web pages.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/detect"
	"github.com/ivantheodoro/disk-recovery-lab/internal/platform"
	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
	"github.com/ivantheodoro/disk-recovery-lab/internal/scan"
)

//go:embed web
var webFS embed.FS

// Options configure a Server.
type Options struct {
	Port    int
	Token   string
	DataDir string // scans/ and images/ live here
	Version string
}

// Server is the running app.
type Server struct {
	opt      Options
	mu       sync.Mutex
	jobs     map[string]*Job
	scans    map[string]*scan.Scan
	scanJob  map[string]*Job
	lastSeen time.Time
	started  time.Time
	http     *http.Server
	// Restart is set when the UI asked for a restart rather than a quit.
	Restart bool
}

// New creates a server.
func New(opt Options) *Server {
	os.MkdirAll(filepath.Join(opt.DataDir, "scans"), 0o755)
	os.MkdirAll(filepath.Join(opt.DataDir, "images"), 0o755)
	platform.ChownToUser(opt.DataDir)
	return &Server{opt: opt, jobs: map[string]*Job{}, scans: map[string]*scan.Scan{}, scanJob: map[string]*Job{},
		lastSeen: time.Now(), started: time.Now()}
}

func (s *Server) scansDir() string  { return filepath.Join(s.opt.DataDir, "scans") }
func (s *Server) imagesDir() string { return filepath.Join(s.opt.DataDir, "images") }

// Listen binds the port, retrying briefly (a restarting instance may still hold it).
func (s *Server) Listen() (net.Listener, error) {
	var err error
	for i := 0; i < 40; i++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.opt.Port)); err == nil {
			return ln, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, err
}

// Serve runs until the UI quits, the app idles out, or ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.http.Shutdown(context.Background())
				return
			case <-t.C:
				s.mu.Lock()
				idle := time.Since(s.lastSeen)
				s.mu.Unlock()
				if idle > 10*time.Minute && !s.anyRunning() { // UI closed and nothing to do
					s.shutdown(false)
					return
				}
			}
		}
	}()
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.finish()
	return err
}

func (s *Server) shutdown(restart bool) {
	s.mu.Lock()
	s.Restart = restart
	for _, j := range s.jobs {
		j.Cancel() // scans stop cleanly and keep what they found
	}
	s.mu.Unlock()
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.http.Shutdown(context.Background())
	}()
}

// finish waits briefly for cancelled scans to write their last checkpoint.
func (s *Server) finish() {
	for i := 0; i < 20 && s.anyRunning(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range s.scans {
		if sc.Meta().State == "scanning" {
			sc.SetStage("cancelled", "Stopped when the app was closed (results so far are kept)")
			sc.Save(s.scansDir())
		}
	}
	platform.ChownToUser(s.opt.DataDir)
}

// ---------------------------------------------------------------- HTTP

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host != fmt.Sprintf("127.0.0.1:%d", s.opt.Port) && host != fmt.Sprintf("localhost:%d", s.opt.Port) {
		http.Error(w, "wrong host", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/api/ping" { // used by the launcher before the token is set
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if t := r.URL.Query().Get("t"); t != "" && r.URL.Path == "/" {
		if t == s.opt.Token {
			http.SetCookie(w, &http.Cookie{Name: "drl", Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}
	if c, err := r.Cookie("drl"); err != nil || c.Value != s.opt.Token {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Disk Recovery Lab</title><body style="font:15px system-ui;padding:40px">`+
			`<h2>Open Disk Recovery Lab from its app icon</h2><p>For safety this page only works in the window the app opens.</p>`)
		return
	}
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		sub, _ := fs.Sub(webFS, "web")
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	case strings.HasPrefix(r.URL.Path, "/preview/"):
		s.preview(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, 415, map[string]any{"error": "JSON only"})
			return
		}
		body := map[string]any{}
		if r.Method == http.MethodPost {
			json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		}
		res, err := s.api(r.Method, r.URL.Path, r.URL.Query(), body)
		switch {
		case errors.Is(err, errNotFound):
			writeJSON(w, 404, map[string]any{"error": err.Error()})
		case err != nil:
			writeJSON(w, 400, map[string]any{"error": err.Error()})
		default:
			writeJSON(w, 200, res)
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

var errNotFound = errors.New("not found")

func str(m map[string]any, k string) string  { v, _ := m[k].(string); return strings.TrimSpace(v) }
func boolOf(m map[string]any, k string) bool { v, _ := m[k].(bool); return v }
func intOf(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// sourcePath validates a drive or image path.
func (s *Server) sourcePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("choose a drive or an image file first")
	}
	if rawdisk.IsDevicePath(p) || !strings.ContainsAny(p, `/\`) {
		if !platform.IsAdmin() {
			return "", errors.New("reading a drive needs administrator rights: use 'Restart as administrator'")
		}
		return rawdisk.DevicePath(p), nil
	}
	if strings.HasPrefix(p, "~") {
		p = filepath.Join(platform.HomeDir(), p[1:])
	}
	abs, _ := filepath.Abs(p)
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return "", fmt.Errorf("not a drive or image file: %s", abs)
	}
	return abs, nil
}

func (s *Server) getScan(id string) (*scan.Scan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc, ok := s.scans[id]; ok {
		return sc, nil
	}
	if !regexp.MustCompile(`^[\w-]+$`).MatchString(id) {
		return nil, errNotFound
	}
	sc, err := scan.Load(s.scansDir(), id)
	if err != nil {
		return nil, errNotFound
	}
	s.scans[id] = sc
	return sc, nil
}

func (s *Server) scanView(sc *scan.Scan) map[string]any {
	m := sc.Meta()
	out := map[string]any{"id": m.ID, "source": m.Source, "label": m.Label, "created": m.Created, "state": m.State,
		"stage": m.Stage, "notes": m.Notes, "files": m.Files, "bytes": m.Bytes, "running": false, "progress": 1.0,
		"error": nil, "elapsed": nil}
	s.mu.Lock()
	j := s.scanJob[sc.ID]
	s.mu.Unlock()
	if j != nil {
		v := j.View()
		out["running"], out["progress"], out["error"], out["elapsed"] = v["state"] == "running", v["progress"], v["error"], v["elapsed"]
	}
	return out
}

func appVersion(s *Server) string { return fmt.Sprintf("%s-%d", s.opt.Version, s.started.Unix()) }

func sortedMetas(dir string, live map[string]*scan.Scan, view func(*scan.Scan) map[string]any) []map[string]any {
	ents, _ := os.ReadDir(dir)
	var out []map[string]any
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".meta.json")
		if sc, ok := live[id]; ok {
			out = append(out, view(sc))
			continue
		}
		m, err := scan.LoadMeta(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		b, _ := json.Marshal(m)
		var v map[string]any
		json.Unmarshal(b, &v)
		v["running"] = false
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a]["created"].(float64) > out[b]["created"].(float64) })
	return out
}

var _ = detect.Human
