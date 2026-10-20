package server

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/detect"
	"github.com/ivantheodoro/disk-recovery-lab/internal/platform"
	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
	"github.com/ivantheodoro/disk-recovery-lab/internal/scan"
)

var scanRoute = regexp.MustCompile(`^/api/scans/([\w-]+)(?:/(\w+))?$`)
var jobRoute = regexp.MustCompile(`^/api/jobs/(\w+)(/cancel)?$`)

func (s *Server) api(method, path string, q url.Values, body map[string]any) (any, error) {
	switch {
	case path == "/api/status":
		return map[string]any{"root": platform.IsAdmin(), "version": appVersion(s), "features": platform.Supported(),
			"data_dir": s.opt.DataDir, "images_dir": s.imagesDir()}, nil
	case path == "/api/quit" || path == "/api/restart":
		s.shutdown(path == "/api/restart")
		return map[string]any{"ok": true}, nil
	case path == "/api/elevate":
		return s.elevate()
	case path == "/api/drives":
		return platform.Drives()
	case path == "/api/scans" && method == http.MethodGet:
		s.mu.Lock()
		live := map[string]*scan.Scan{}
		for k, v := range s.scans {
			live[k] = v
		}
		s.mu.Unlock()
		out := sortedMetas(s.scansDir(), live, s.scanView)
		if out == nil {
			out = []map[string]any{}
		}
		return out, nil
	case path == "/api/scans" && method == http.MethodPost:
		return s.newScan(body)
	case scanRoute.MatchString(path):
		m := scanRoute.FindStringSubmatch(path)
		sc, err := s.getScan(m[1])
		if err != nil {
			return nil, err
		}
		return s.scanAction(sc, m[2], q, body)
	case jobRoute.MatchString(path):
		m := jobRoute.FindStringSubmatch(path)
		s.mu.Lock()
		j := s.jobs[m[1]]
		s.mu.Unlock()
		if j == nil {
			return nil, errNotFound
		}
		if m[2] != "" {
			j.Cancel()
		}
		return j.View(), nil
	case path == "/api/probe":
		src, err := s.sourcePath(str(body, "path"))
		if err != nil {
			return nil, err
		}
		return detect.Diagnose(src)
	case path == "/api/mount":
		id := str(body, "id")
		if id == "" {
			return nil, errors.New("choose a partition first")
		}
		return platform.MountReadOnly(id), nil
	case path == "/api/unmount":
		out, err := platform.Unmount(str(body, "id"), boolOf(body, "force"))
		return map[string]any{"output": out}, err
	case path == "/api/attach":
		src, err := s.sourcePath(str(body, "path"))
		if err != nil {
			return nil, err
		}
		mount := true
		if v, ok := body["mount"].(bool); ok {
			mount = v
		}
		return platform.AttachImage(src, mount)
	case path == "/api/detach":
		out, err := platform.Detach(str(body, "dev"))
		return map[string]any{"output": out}, err
	case path == "/api/fsck":
		return s.fsck(str(body, "id"))
	case path == "/api/reveal":
		return map[string]any{"ok": true}, platform.Reveal(str(body, "path"))
	case path == "/api/testdisk":
		return platform.CreatePractice(s.imagesDir(), str(body, "kind"))
	case path == "/api/tools":
		return s.tool(body)
	}
	return nil, errNotFound
}

func (s *Server) elevate() (any, error) {
	if platform.IsAdmin() {
		return map[string]any{"ok": true, "already": true}, nil
	}
	tf, err := writeTokenFile(s.opt.Token)
	if err != nil {
		return nil, err
	}
	uid, gid, home := platform.CurrentUser()
	if err := platform.Elevate(platform.ServeArgs{Port: s.opt.Port, TokenFile: tf, DataDir: s.opt.DataDir, Home: home,
		UID: uid, GID: gid}); err != nil {
		os.Remove(tf)
		return nil, err
	}
	s.shutdown(false) // the elevated copy takes over the same port and token
	return map[string]any{"ok": true}, nil
}

// WriteTokenFile stores the session token in a private temp file for the
// elevated child (command lines are visible to other users; files are not).
func writeTokenFile(token string) (string, error) {
	f, err := os.CreateTemp("", "drl-token-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	f.Chmod(0o600)
	_, err = f.WriteString(token)
	return f.Name(), err
}

// WriteTokenFile is exported for the launcher.
var WriteTokenFile = writeTokenFile

func (s *Server) newScan(body map[string]any) (any, error) {
	src, err := s.sourcePath(str(body, "path"))
	if err != nil {
		return nil, err
	}
	label := str(body, "label")
	if label == "" {
		label = filepath.Base(src)
	}
	sc := scan.New(src, label)
	sc.Offset, sc.Length = intOf(body, "offset"), intOf(body, "length")
	deep := boolOf(body, "deep")
	j := s.startJob("scan", map[string]any{"scan": sc.ID}, func(j *Job) (any, error) {
		defer platform.ChownToUser(s.scansDir())
		return map[string]any{"scan": sc.ID}, scan.Run(sc, j, deep, s.scansDir())
	})
	s.mu.Lock()
	s.scans[sc.ID], s.scanJob[sc.ID] = sc, j
	s.mu.Unlock()
	return s.scanView(sc), nil
}

func (s *Server) scanAction(sc *scan.Scan, action string, q url.Values, body map[string]any) (any, error) {
	switch action {
	case "":
		return s.scanView(sc), nil
	case "cancel":
		s.mu.Lock()
		if j := s.scanJob[sc.ID]; j != nil {
			j.Cancel()
		}
		s.mu.Unlock()
		return s.scanView(sc), nil
	case "delete":
		s.mu.Lock()
		j := s.scanJob[sc.ID]
		s.mu.Unlock()
		if j != nil && j.Running() {
			return nil, errors.New("stop the scan first")
		}
		for _, ext := range []string{".json.gz", ".meta.json"} {
			os.Remove(filepath.Join(s.scansDir(), sc.ID+ext))
		}
		s.mu.Lock()
		delete(s.scans, sc.ID)
		s.mu.Unlock()
		return map[string]any{"ok": true}, nil
	case "items":
		return listItems(sc, q), nil
	case "summary":
		return summary(sc), nil
	case "destinations":
		return platform.Destinations(sc.Source), nil
	case "recover":
		dest := str(body, "dest")
		if strings.HasPrefix(dest, "~") {
			dest = filepath.Join(platform.HomeDir(), dest[1:])
		}
		dest, _ = filepath.Abs(dest)
		if st, err := os.Stat(dest); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("destination folder does not exist: %s", dest)
		}
		if platform.SameDisk(sc.Source, dest) {
			return nil, errors.New("that destination is on the drive you are recovering from. Saving there could " +
				"overwrite the very files you want back. Pick another drive")
		}
		var ids []int
		if a, ok := body["ids"].([]any); ok {
			for _, v := range a {
				if f, ok := v.(float64); ok {
					ids = append(ids, int(f))
				}
			}
		}
		j := s.startJob("recover", map[string]any{"scan": sc.ID, "dest": dest}, func(j *Job) (any, error) {
			res, err := scan.Recover(sc, ids, dest, j)
			if res != nil {
				platform.ChownToUser(res.OutDir)
			}
			return res, err
		})
		return j.View(), nil
	}
	return nil, errNotFound
}

func status(it *scan.Item) string {
	switch {
	case it.Origin == "carved":
		return "carved"
	case it.Deleted && strings.Contains(it.Note, "overwritten"):
		return "risk"
	case it.Deleted:
		return "deleted"
	case it.Note != "":
		return "warn"
	}
	return "ok"
}

var previewExt = map[string]string{"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"bmp": "image/bmp", "webp": "image/webp", "heic": "image/heic", "tif": "image/tiff", "tiff": "image/tiff",
	"pdf": "application/pdf", "mp4": "video/mp4", "mov": "video/quicktime", "m4v": "video/mp4", "mp3": "audio/mpeg",
	"wav": "audio/wav", "m4a": "audio/mp4", "aac": "audio/aac", "txt": "text/plain", "md": "text/plain",
	"csv": "text/plain", "vmx": "text/plain", "json": "text/plain", "xml": "text/plain", "log": "text/plain",
	"html": "text/plain", "plist": "text/plain"}

func itemView(sc *scan.Scan, it *scan.Item, flat bool) map[string]any {
	ext := scan.Ext(it.Name)
	if it.Kind != "file" {
		ext = ""
	}
	v := map[string]any{"id": it.ID, "name": it.Name, "kind": it.Kind, "size": it.Size, "count": it.Count,
		"deleted": it.Deleted, "mtime": it.MTime, "origin": it.Origin, "note": it.Note, "group": it.Group,
		"ext": ext, "preview": previewExt[ext] != "" && it.Kind == "file", "status": status(it)}
	if flat {
		p := sc.PathOf(it.ID)
		v["where"] = strings.Join(p[:len(p)-1], "/")
	}
	return v
}

func listItems(sc *scan.Scan, q url.Values) map[string]any {
	folder, _ := strconv.Atoi(q.Get("folder"))
	group, text := q.Get("group"), strings.ToLower(q.Get("q"))
	onlyDel, showHidden := q.Get("deleted") == "1", q.Get("hidden") == "1"
	sortKey, desc := q.Get("sort"), q.Get("dir") == "desc"
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	flat := group != "" || text != "" || onlyDel
	var out map[string]any
	sc.Read(func(sc *scan.Scan) {
		var rows []*scan.Item
		crumbs := []map[string]any{}
		if flat {
			for _, it := range sc.Items {
				if it.Kind == "file" && (group == "" || it.Group == group) && (text == "" || strings.Contains(strings.ToLower(it.Name), text)) &&
					(!onlyDel || it.Deleted) && (showHidden || !strings.HasPrefix(it.Name, ".")) {
					rows = append(rows, it)
				}
			}
		} else {
			if folder < 0 || folder >= len(sc.Items) {
				folder = 0
			}
			if kids := sc.Children[folder]; folder == 0 && len(kids) == 1 && sc.Items[kids[0]].Kind == "dir" {
				folder = kids[0] // single partition: open it directly
			}
			for _, c := range sc.Children[folder] {
				if it := sc.Items[c]; showHidden || !strings.HasPrefix(it.Name, ".") {
					rows = append(rows, it)
				}
			}
			chain := []map[string]any{}
			for i := folder; i > 0; i = *sc.Items[i].Parent {
				chain = append([]map[string]any{{"id": i, "name": sc.Items[i].Name}}, chain...)
			}
			crumbs = append([]map[string]any{{"id": 0, "name": sc.Items[0].Name}}, chain...)
		}
		less := map[string]func(a, b *scan.Item) bool{
			"size":  func(a, b *scan.Item) bool { return a.Size < b.Size },
			"items": func(a, b *scan.Item) bool { return a.Count < b.Count },
			"date":  func(a, b *scan.Item) bool { return mt(a) < mt(b) },
			"type": func(a, b *scan.Item) bool {
				return scan.Ext(a.Name)+"\x00"+strings.ToLower(a.Name) < scan.Ext(b.Name)+"\x00"+strings.ToLower(b.Name)
			},
			"status": func(a, b *scan.Item) bool {
				return status(a)+"\x00"+strings.ToLower(a.Name) < status(b)+"\x00"+strings.ToLower(b.Name)
			},
		}[sortKey]
		if less == nil {
			less = func(a, b *scan.Item) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) }
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if (rows[i].Kind == "dir") != (rows[j].Kind == "dir") {
				return rows[i].Kind == "dir"
			}
			if desc {
				return less(rows[j], rows[i])
			}
			return less(rows[i], rows[j])
		})
		page := []map[string]any{}
		for i := offset; i < len(rows) && i < offset+limit; i++ {
			page = append(page, itemView(sc, rows[i], flat))
		}
		out = map[string]any{"total": len(rows), "crumbs": crumbs, "folder": folder, "items": page}
	})
	return out
}

func mt(it *scan.Item) int64 {
	if it.MTime == nil {
		return 0
	}
	return *it.MTime
}

func summary(sc *scan.Scan) map[string]any {
	counts := map[string][2]int64{}
	var del, delSize int64
	sc.Read(func(sc *scan.Scan) {
		for _, it := range sc.Items {
			if it.Kind != "file" {
				continue
			}
			c := counts[it.Group]
			counts[it.Group] = [2]int64{c[0] + 1, c[1] + it.Size}
			if it.Deleted {
				del++
				delSize += it.Size
			}
		}
	})
	groups := []map[string]any{}
	for _, g := range scan.Groups {
		groups = append(groups, map[string]any{"name": g, "count": counts[g][0], "size": counts[g][1]})
	}
	return map[string]any{"groups": groups, "deleted": del, "deleted_size": delSize}
}

func (s *Server) fsck(id string) (any, error) {
	if id == "" {
		return nil, errors.New("choose a partition first")
	}
	if !platform.IsAdmin() {
		return nil, errors.New("checking a drive needs administrator rights")
	}
	cmd, err := platform.FsckCommand(id)
	if err != nil {
		return nil, err
	}
	j := s.startJob("fsck", map[string]any{"id": id}, func(j *Job) (any, error) {
		j.Log("$ " + strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		out, _ := c.StdoutPipe()
		c.Stderr = c.Stdout
		if err := c.Start(); err != nil {
			return nil, err
		}
		var lines []string
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if j.Check() != nil {
				c.Process.Kill()
				break
			}
			lines = append(lines, sc.Text())
			j.Log(sc.Text())
		}
		err := c.Wait()
		if e := j.Check(); e != nil {
			return nil, e
		}
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if len(lines) > 400 {
			lines = lines[len(lines)-400:]
		}
		return map[string]any{"command": strings.Join(cmd, " "), "exit": code, "ok": code == 0, "filesystem": "", "output": lines}, nil
	})
	return j.View(), nil
}

func (s *Server) tool(body map[string]any) (any, error) {
	src, err := s.sourcePath(str(body, "path"))
	if err != nil {
		return nil, err
	}
	out := func(def string) (string, error) {
		p := str(body, "out")
		if p == "" {
			p = filepath.Join(s.imagesDir(), def+"-"+time.Now().Format("20060102-150405")+".img")
		}
		p, _ = filepath.Abs(p)
		if p == src {
			return "", errors.New("the output must differ from the source")
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if platform.SameDisk(src, filepath.Dir(p)) {
			return "", errors.New("refusing to write the image onto the disk being imaged")
		}
		return p, nil
	}
	switch str(body, "kind") {
	case "lost":
		return s.startJob("lost", map[string]any{"path": src}, func(j *Job) (any, error) {
			f, err := detect.SearchLostPartitions(src, j)
			if f == nil {
				f = []detect.Found{}
			}
			return map[string]any{"partitions": f}, err
		}).View(), nil
	case "image", "repair":
		kind := str(body, "kind")
		dst, err := out(map[string]string{"image": "image", "repair": "repaired"}[kind])
		if err != nil {
			return nil, err
		}
		return s.startJob(kind, map[string]any{"path": src, "out": dst}, func(j *Job) (any, error) {
			defer platform.ChownToUser(dst)
			if kind == "image" {
				return detect.MakeImage(src, dst, j)
			}
			return detect.RepairedCopy(src, dst, intOf(body, "part_start"), intOf(body, "backup_off"), str(body, "fs"), j)
		}).View(), nil
	}
	return nil, errNotFound
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/preview/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	sc, err := s.getScan(parts[0])
	id, _ := strconv.Atoi(parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var it scan.Item
	ok := false
	sc.Read(func(sc *scan.Scan) {
		if id >= 0 && id < len(sc.Items) {
			it, ok = *sc.Items[id], true
		}
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	const limit = 64 << 20
	rd, err := rawdisk.Open(sc.Source)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer rd.Close()
	var buf []byte
	left := it.Size
	if left > limit {
		left = limit
	}
	for _, rg := range it.Ranges {
		if left <= 0 {
			break
		}
		n := rg[1]
		if n > left {
			n = left
		}
		b, err := rd.Read(rg[0], int(n))
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		buf = append(buf, b...)
		left -= n
	}
	if mime := previewExt[scan.Ext(it.Name)]; mime != "" && it.Size <= limit {
		w.Header().Set("Content-Type", mime)
		if mime != "application/pdf" {
			w.Header().Set("Content-Security-Policy", "sandbox") // previewed content cannot run scripts
		}
		w.Write(buf)
		return
	}
	head := buf
	if len(head) > 1024 {
		head = head[:1024]
	}
	var sb strings.Builder
	sb.WriteString("No preview for this file type. First bytes:\n\n")
	for i := 0; i < len(head); i += 16 {
		e := i + 16
		if e > len(head) {
			e = len(head)
		}
		hexs, asc := "", ""
		for _, c := range head[i:e] {
			hexs += fmt.Sprintf("%02x ", c)
			if c >= 32 && c < 127 {
				asc += string(rune(c))
			} else {
				asc += "."
			}
		}
		fmt.Fprintf(&sb, "%08x  %-48s %s\n", i, hexs, asc)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(sb.String()))
}
