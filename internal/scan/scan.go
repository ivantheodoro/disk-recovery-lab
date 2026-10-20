// Package scan builds a browsable, saved index of recoverable files without
// writing to the source, and later recovers a chosen subset.
//
// Two sources feed a scan: filesystem metadata ("quick scan", real names and
// folders) and signature carving over free space ("deep scan").
package scan

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Groups are the file categories shown in the sidebar, in display order.
var Groups = []string{"Pictures", "Video", "Audio", "Documents", "Archives", "Virtual machines", "Other"}

var groupExt = map[string]string{
	"Pictures":         "jpg jpeg png gif bmp tif tiff heic heif webp raw cr2 cr3 nef arw dng orf rw2 psd svg ico",
	"Video":            "mov mp4 m4v avi mkv wmv mpg mpeg 3gp mts m2ts webm flv",
	"Audio":            "mp3 wav aac m4a flac aiff aif ogg wma caf",
	"Documents":        "pdf doc docx xls xlsx ppt pptx key pages numbers txt rtf md csv odt ods odp html htm xml json sqlite db epub",
	"Archives":         "zip rar 7z gz tgz bz2 xz tar dmg iso pkg sparseimage sparsebundle",
	"Virtual machines": "vmdk vmx vmxf vmsd vmsn vmem nvram vmss vdi vhd vhdx hdd hds pvm pvs qcow2 utm",
}
var ext2group = func() map[string]string {
	m := map[string]string{}
	for g, list := range groupExt {
		for _, e := range strings.Fields(list) {
			m[e] = g
		}
	}
	return m
}()

// Ext returns the lower-case extension of a file name, without the dot.
func Ext(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return strings.ToLower(name[i+1:])
	}
	return ""
}

// GroupOf maps a file name to its category.
func GroupOf(name string) string {
	if g, ok := ext2group[Ext(name)]; ok {
		return g
	}
	return "Other"
}

// Item is a file or folder in the scan tree. The JSON layout is shared with
// the original prototype so saved scans stay compatible.
type Item struct {
	ID      int        `json:"id"`
	Parent  *int       `json:"parent"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"` // "file" or "dir"
	Size    int64      `json:"size"`
	Count   int        `json:"count"`
	Deleted bool       `json:"deleted"`
	MTime   *int64     `json:"mtime"`
	Origin  string     `json:"origin"` // "fs" or "carved"
	Note    string     `json:"note"`
	Group   string     `json:"group"`
	Ranges  [][2]int64 `json:"ranges,omitempty"`
}

// Meta is the small summary stored next to a scan.
type Meta struct {
	ID      string   `json:"id"`
	Source  string   `json:"source"`
	Offset  int64    `json:"offset,omitempty"`
	Length  int64    `json:"length,omitempty"`
	Label   string   `json:"label"`
	Created float64  `json:"created"`
	State   string   `json:"state"`
	Stage   string   `json:"stage"`
	Notes   []string `json:"notes"`
	Files   int      `json:"files"`
	Bytes   int64    `json:"bytes"`
}

// Scan is one scan session.
type Scan struct {
	mu       sync.RWMutex
	ID       string
	Source   string
	Offset   int64 // optional: restrict to one partition of Source
	Length   int64
	Label    string
	Created  float64
	State    string
	Stage    string
	Notes    []string
	Items    []*Item
	Children map[int][]int
}

// New creates an empty scan whose root folder is named after label.
func New(source, label string) *Scan {
	b := make([]byte, 2)
	rand.Read(b)
	s := &Scan{ID: time.Now().Format("20060102-150405-") + hex.EncodeToString(b), Source: source, Label: label,
		Created: float64(time.Now().UnixNano()) / 1e9, State: "scanning", Notes: []string{}, Children: map[int][]int{}}
	s.Add(label, "dir", -1, 0, nil, false, nil, "fs", "")
	return s
}

// Add appends an item (parent -1 = root) and rolls sizes up to ancestors.
func (s *Scan) Add(name, kind string, parent int, size int64, ranges [][2]int64, deleted bool, mtime *int64, origin, note string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := &Item{ID: len(s.Items), Name: name, Kind: kind, Deleted: deleted, MTime: mtime, Origin: origin, Note: note, Ranges: ranges}
	if parent >= 0 {
		p := parent
		it.Parent = &p
		s.Children[parent] = append(s.Children[parent], it.ID)
	}
	if kind == "file" {
		it.Size, it.Group = size, GroupOf(name)
		for p := it.Parent; p != nil; p = s.Items[*p].Parent {
			s.Items[*p].Size += size
			s.Items[*p].Count++
		}
	}
	s.Items = append(s.Items, it)
	return it.ID
}

// Folder returns the child folder called name, creating it if needed.
func (s *Scan) Folder(parent int, name string) int {
	s.mu.RLock()
	for _, c := range s.Children[parent] {
		if it := s.Items[c]; it.Kind == "dir" && it.Name == name {
			s.mu.RUnlock()
			return c
		}
	}
	s.mu.RUnlock()
	return s.Add(name, "dir", parent, 0, nil, false, nil, "fs", "")
}

// Rename changes an item's name.
func (s *Scan) Rename(id int, name string) {
	s.mu.Lock()
	s.Items[id].Name = name
	s.mu.Unlock()
}

// Note appends a human-readable note about the scan.
func (s *Scan) Note(n string) {
	s.mu.Lock()
	s.Notes = append(s.Notes, n)
	s.mu.Unlock()
}

// SetStage updates the current stage text.
func (s *Scan) SetStage(state, stage string) {
	s.mu.Lock()
	if state != "" {
		s.State = state
	}
	s.Stage = stage
	s.mu.Unlock()
}

// Read runs fn with the scan locked for reading.
func (s *Scan) Read(fn func(s *Scan)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s)
}

// PathOf returns the path components of an item below the root.
func (s *Scan) PathOf(id int) []string {
	var parts []string
	for i := id; i > 0; i = *s.Items[i].Parent {
		parts = append([]string{s.Items[i].Name}, parts...)
	}
	return parts
}

// Descendants returns every file under id (id itself if it is a file).
func (s *Scan) Descendants(id int) []int {
	var out []int
	stack := []int{id}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.Items[i].Kind == "file" {
			out = append(out, i)
		}
		stack = append(stack, s.Children[i]...)
	}
	return out
}

// Meta returns the scan summary.
func (s *Scan) Meta() Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metaLocked()
}

func (s *Scan) metaLocked() Meta {
	root := s.Items[0]
	return Meta{ID: s.ID, Source: s.Source, Offset: s.Offset, Length: s.Length, Label: s.Label, Created: s.Created,
		State: s.State, Stage: s.Stage, Notes: append([]string{}, s.Notes...), Files: root.Count, Bytes: root.Size}
}

// Save writes <id>.json.gz and <id>.meta.json atomically into dir.
func (s *Scan) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tmp := filepath.Join(dir, s.ID+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	err = json.NewEncoder(zw).Encode(map[string]any{"meta": s.metaLocked(), "items": s.Items})
	if e := zw.Close(); err == nil {
		err = e
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, s.ID+".json.gz")); err != nil {
		return err
	}
	mb, _ := json.Marshal(s.metaLocked())
	return os.WriteFile(filepath.Join(dir, s.ID+".meta.json"), mb, 0o644)
}

// Load reads a saved scan.
func Load(dir, id string) (*Scan, error) {
	f, err := os.Open(filepath.Join(dir, id+".json.gz"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var d struct {
		Meta  Meta    `json:"meta"`
		Items []*Item `json:"items"`
	}
	if err := json.NewDecoder(zr).Decode(&d); err != nil {
		return nil, err
	}
	m := d.Meta
	s := &Scan{ID: m.ID, Source: m.Source, Offset: m.Offset, Length: m.Length, Label: m.Label, Created: m.Created,
		State: m.State, Stage: m.Stage, Notes: m.Notes, Items: d.Items, Children: map[int][]int{}}
	if s.State == "scanning" {
		s.State = "interrupted"
	}
	if s.Notes == nil {
		s.Notes = []string{}
	}
	for _, it := range s.Items {
		if it.Parent != nil {
			s.Children[*it.Parent] = append(s.Children[*it.Parent], it.ID)
		}
	}
	return s, nil
}

// LoadMeta reads only the summary of a saved scan.
func LoadMeta(path string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	if m.State == "scanning" {
		m.State = "interrupted"
	}
	return m, err
}
