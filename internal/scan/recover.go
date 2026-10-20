package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/detect"
	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

// RecoverResult summarises a recovery.
type RecoverResult struct {
	OutDir string   `json:"outdir"`
	Files  int      `json:"files"`
	Bytes  int64    `json:"bytes"`
	Errors []string `json:"errors"`
}

var winReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "LPT1": true, "LPT2": true, "LPT3": true}

// SafeName makes a recovered name valid on the destination filesystem.
func SafeName(name string) string {
	name = strings.NewReplacer("/", ":", "\x00", "").Replace(name)
	if runtime.GOOS == "windows" {
		name = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`<>:"\|?*`, r) || r < 32 {
				return '_'
			}
			return r
		}, name)
		name = strings.TrimRight(name, ". ")
		if base := strings.ToUpper(strings.SplitN(name, ".", 2)[0]); winReserved[base] {
			name = "_" + name
		}
	}
	if name == "" || name == "." || name == ".." {
		name = "_" + name
	}
	return name
}

// FreeSpace is set by the platform layer; it returns free bytes at a path.
var FreeSpace = func(path string) (int64, error) { return -1, nil }

// Recover copies the chosen items (folders include everything below them)
// into a new "Recovered <date>" folder inside dest.
func Recover(s *Scan, ids []int, dest string, job Job) (*RecoverResult, error) {
	set := map[int]bool{}
	var total int64
	s.Read(func(s *Scan) {
		for _, id := range ids {
			if id < 0 || id >= len(s.Items) {
				continue
			}
			for _, f := range s.Descendants(id) {
				if !set[f] {
					set[f] = true
					total += s.Items[f].Size
				}
			}
		}
	})
	files := make([]int, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Ints(files)
	if free, err := FreeSpace(dest); err == nil && free >= 0 && total > free {
		return nil, fmt.Errorf("not enough space at the destination: need %s, %s free", detect.Human(total), detect.Human(free))
	}
	root := filepath.Join(dest, "Recovered "+time.Now().Format("2006-01-02 15.04"))
	r, err := rawdisk.Open(s.Source)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	res := &RecoverResult{OutDir: root, Errors: []string{}}
	for n, fid := range files {
		if err := job.Check(); err != nil {
			return res, err
		}
		var it Item
		var parts []string
		s.Read(func(s *Scan) { it = *s.Items[fid]; parts = s.PathOf(fid) })
		for i := range parts {
			parts[i] = SafeName(parts[i])
		}
		dst := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, err
		}
		ext := filepath.Ext(dst)
		stem := strings.TrimSuffix(dst, ext)
		for k := 1; ; k++ {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				break
			}
			dst = fmt.Sprintf("%s (%d)%s", stem, k, ext)
		}
		out, err := os.Create(dst)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", strings.Join(parts, "/"), err))
			continue
		}
		bad := 0
		for _, rg := range it.Ranges {
			for p := rg[0]; p < rg[0]+rg[1]; {
				if err := job.Check(); err != nil {
					out.Close()
					return res, err
				}
				n := int64(4 << 20)
				if rg[0]+rg[1]-p < n {
					n = rg[0] + rg[1] - p
				}
				b, err := r.Read(p, int(n))
				if err != nil || int64(len(b)) < n {
					b, bad = make([]byte, n), bad+1
				}
				if _, err := out.Write(b); err != nil {
					out.Close()
					return res, err
				}
				p += n
				res.Bytes += n
				if total > 0 {
					job.SetProgress(float64(res.Bytes) / float64(total))
				}
			}
		}
		out.Close()
		if it.MTime != nil {
			t := time.Unix(*it.MTime, 0)
			os.Chtimes(dst, t, t)
		}
		res.Files++
		if bad > 0 {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %d unreadable chunk(s) zero-filled", strings.Join(parts, "/"), bad))
		}
		if n%20 == 0 {
			job.Log(fmt.Sprintf("Recovered %d of %d files", n+1, len(files)))
		}
	}
	job.Log(fmt.Sprintf("Recovered %d files (%s)", res.Files, detect.Human(res.Bytes)))
	return res, nil
}
