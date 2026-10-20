package scan

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/carve"
	"github.com/ivantheodoro/disk-recovery-lab/internal/detect"
	"github.com/ivantheodoro/disk-recovery-lab/internal/hfs"
	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

// ErrCancelled is returned by Job.Check after the user stops an operation.
var ErrCancelled = errors.New("cancelled")

// Job reports progress for long operations.
type Job interface {
	Check() error
	Log(string)
	SetProgress(float64)
	SetFound(int)
}

var hidden = map[string]bool{".journal": true, ".journal_info_block": true, ".fseventsd": true,
	".Spotlight-V100": true, ".HFS+ Private Directory Data\r": true, "\x00\x00\x00\x00HFS+ Private Data": true,
	".hotfiles.btree": true}

type volume struct {
	offset, size int64
	name         string
	fs           *detect.FS
	root         int
	hfs          *hfs.Volume
}

type lostJob struct{ Job }

func (lostJob) SetProgress(float64) {}

func findVolumes(r *rawdisk.Reader, s *Scan, job Job) []*volume {
	if s.Offset > 0 || s.Length > 0 { // one partition of a whole-disk source
		ln := s.Length
		if ln == 0 {
			ln = r.Size - s.Offset
		}
		return []*volume{{offset: s.Offset, size: ln, fs: detect.DetectFS(r, s.Offset)}}
	}
	_, parts := detect.PartitionTable(r)
	var vols []*volume
	for _, p := range parts {
		if p.Type == "EFI System" || p.Type == "Microsoft Reserved" {
			continue
		}
		vols = append(vols, &volume{offset: p.Offset, size: p.Size, name: p.Name, fs: detect.DetectFS(r, p.Offset)})
	}
	if len(vols) == 0 {
		if fs := detect.DetectFS(r, 0); fs != nil {
			vols = append(vols, &volume{offset: 0, size: r.Size, fs: fs})
		}
	}
	any := false
	for _, v := range vols {
		any = any || v.fs != nil
	}
	if !any {
		job.Log("No readable partition map; searching for lost partitions")
		s.Note("No readable partition map; partitions were found by searching the disk")
		found, _ := detect.SearchLostPartitions(s.Source, lostJob{job})
		vols = nil
		for _, f := range found {
			start := f.Offset
			if f.BackupOf != nil {
				start = *f.BackupOf
			}
			vols = append(vols, &volume{offset: start, size: r.Size - start, name: "Lost partition", fs: f.FS})
		}
	}
	return vols
}

// Run scans s.Source. With deep, free space is also carved by signature.
// Results are checkpointed to dir every 30 seconds and when finished.
func Run(s *Scan, job Job, deep bool, dir string) error {
	r, err := rawdisk.Open(s.Source)
	if err != nil {
		s.SetStage("error", err.Error())
		s.Save(dir)
		return err
	}
	defer r.Close()
	last := time.Now()
	stage := func(name string, frac float64) error {
		if err := job.Check(); err != nil {
			return err
		}
		s.SetStage("", name)
		job.SetProgress(frac)
		job.SetFound(s.Meta().Files)
		if time.Since(last) > 30*time.Second {
			last = time.Now()
			s.Save(dir)
		}
		return nil
	}
	err = run(r, s, job, deep, stage)
	switch {
	case errors.Is(err, ErrCancelled):
		s.SetStage("cancelled", "Stopped by user (results so far are kept)")
		err = nil
	case err != nil:
		s.SetStage("error", err.Error())
	default:
		s.SetStage("done", "Complete")
	}
	job.SetFound(s.Meta().Files)
	if e := s.Save(dir); err == nil {
		err = e
	}
	return err
}

func run(r *rawdisk.Reader, s *Scan, job Job, deep bool, stage func(string, float64) error) error {
	if err := stage("Reading partition map", 0); err != nil {
		return err
	}
	vols := findVolumes(r, s, job)
	quickShare := 1.0
	if deep {
		quickShare = 0.3
	}
	for n, v := range vols {
		fsName := "unknown"
		if v.fs != nil {
			fsName = v.fs.Name
		}
		name := v.name
		if name == "" {
			name = fmt.Sprintf("Partition %d", n+1)
		}
		label := fmt.Sprintf("%s (%s, %s)", name, fsName, detect.Human(v.size))
		v.root = s.Add(label, "dir", 0, 0, nil, false, nil, "fs", "")
		if fsName != "HFS+" && fsName != "HFSX" {
			s.Note(fmt.Sprintf("%s: a file list for %s is not supported yet; deep scan finds files by content", label, fsName))
			continue
		}
		vol, err := hfs.Open(r, v.offset)
		if err == nil {
			err = populateHFS(s, v.root, vol, func(name string, f float64) error {
				return stage(name, 0.02+(quickShare-0.02)*f)
			})
		}
		if errors.Is(err, ErrCancelled) {
			return err
		}
		if err != nil {
			s.Note(fmt.Sprintf("%s: catalog unreadable (%v); use deep scan", label, err))
			job.Log("Catalog unreadable on " + label + ": " + err.Error())
			continue
		}
		v.hfs = vol
		s.Read(func(s *Scan) {
			job.Log(fmt.Sprintf("%s: %d files listed", s.Items[v.root].Name, s.Items[v.root].Count))
		})
	}
	if !deep {
		return nil
	}
	type region struct {
		a, b int64
		root int
	}
	var regions []region
	for _, v := range vols {
		if v.hfs != nil {
			free := v.hfs.FreeRuns()
			var total int64
			for _, f := range free {
				regions = append(regions, region{f[0], f[0] + f[1], v.root})
				total += f[1]
			}
			s.Note(fmt.Sprintf("Deep scan limited to %s of free space", detect.Human(total)))
			continue
		}
		regions = append(regions, region{v.offset, v.offset + v.size, v.root})
	}
	if len(regions) == 0 {
		regions = append(regions, region{0, r.Size, s.Add("Whole disk", "dir", 0, 0, nil, false, nil, "fs", "")})
	}
	var total, done int64
	for _, rg := range regions {
		total += rg.b - rg.a
	}
	if total == 0 {
		total = 1
	}
	var stageErr error
	for _, rg := range regions {
		sig := s.Folder(rg.root, "Found by signature (no names)")
		err := carve.Scan(r, rg.a, rg.b, func() error { return stageErr },
			func(h carve.Hit) {
				g := s.Folder(sig, h.Group)
				s.Add(fmt.Sprintf("file_%012x.%s", h.Offset, h.Ext), "file", g, h.Length, [][2]int64{{h.Offset, h.Length}},
					true, nil, "carved", "Found by signature; original name unknown")
			},
			func(n int64) {
				done += n
				stageErr = stage("Deep scan: searching free space for files by content", quickShare+(1-quickShare)*float64(done)/float64(total))
			})
		if err != nil {
			return err
		}
	}
	return nil
}

func populateHFS(s *Scan, root int, v *hfs.Volume, stage func(string, float64) error) error {
	var stopErr error
	prog := func(name string) func(float64) {
		return func(f float64) {
			if stopErr == nil {
				stopErr = stage(name, f*0.5)
			}
		}
	}
	if err := v.LoadExtentsOverflow(prog("Reading extents overflow")); err != nil {
		return err
	}
	cat, err := v.ReadCatalog(func(f float64) {
		if stopErr == nil {
			stopErr = stage("Reading file list (catalog)", 0.1+f*0.9)
		}
	})
	if stopErr != nil {
		return stopErr
	}
	if err != nil {
		return err
	}
	if v.HeaderDamaged {
		s.Note("The catalog B-tree header is damaged (this is why macOS cannot mount the volume). " +
			"The file list was rebuilt by reading every catalog node directly.")
	}
	live := 0
	for _, f := range cat.Files {
		if f.Live {
			live++
		}
	}
	s.Note(fmt.Sprintf("%s: %d folders, %d files in catalog, %d deleted entries recovered from stale catalog nodes",
		v.Kind, len(cat.Folders), live, len(cat.Files)-live))
	if r := cat.Folders[2]; r != nil && r.Name != "" {
		s.Rename(root, fmt.Sprintf("%s (%s)", r.Name, v.Kind))
	}
	private := map[uint32]bool{}
	hiddenIDs := map[uint32]bool{}
	for id, f := range cat.Folders {
		if f.Name == "\x00\x00\x00\x00HFS+ Private Data" {
			private[id] = true
		}
		if hidden[f.Name] {
			hiddenIDs[id] = true
		}
	}
	for id := range private {
		hiddenIDs[id] = true
	}
	inodes := map[string]*hfs.Record{}
	for _, f := range cat.Files {
		if private[f.Parent] {
			inodes[f.Name] = f
		}
	}
	type child struct {
		dir bool
		rec *hfs.Record
	}
	kids := map[uint32][]child{}
	for _, f := range cat.Folders {
		kids[f.Parent] = append(kids[f.Parent], child{true, f})
	}
	for _, f := range cat.Files {
		kids[f.Parent] = append(kids[f.Parent], child{false, f})
	}
	addFile := func(parent int, f *hfs.Record) {
		note := ""
		ext, size, id := f.Ext, f.Size, f.ID
		if f.Type == "hlnkhfs+" { // hard link: content lives in an iNode file
			if t := inodes[fmt.Sprintf("iNode%d", f.Special)]; t != nil {
				ext, size, id = t.Ext, t.Size, t.ID
			}
		}
		if f.Flags&0x20 != 0 {
			note = "HFS+ compressed file: content is stored in a resource fork (not supported)"
		}
		if f.Live || id != f.ID {
			ext = v.ExtentsFor(id, ext)
		}
		rng := v.Ranges(ext, size)
		var got int64
		for _, x := range rng {
			got += x[1]
		}
		if got < size && note == "" {
			note = "Some extents are missing; the file may be incomplete"
		}
		if !f.Live && len(ext) > 0 {
			lastExt := ext[len(ext)-1]
			if v.IsAllocated(int64(ext[0].Start)) || v.IsAllocated(int64(lastExt.Start)+int64(lastExt.Count)-1) {
				note = "Deleted; its blocks are now used by other data (may be overwritten)"
			} else {
				note = "Deleted; blocks still free (good chance of recovery)"
			}
		}
		var mt *int64
		if f.MTime != 0 {
			t := f.MTime
			mt = &t
		}
		s.Add(f.Name, "file", parent, size, rng, !f.Live, mt, "fs", note)
	}
	idmap := map[uint32]int{2: root}
	queue := []uint32{2}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		list := kids[h]
		sortChildren(list, func(i int) (bool, string) { return list[i].dir, list[i].rec.Name })
		for _, c := range list {
			if hidden[c.rec.Name] || private[c.rec.Parent] {
				continue
			}
			if c.dir {
				if _, done := idmap[c.rec.ID]; done {
					continue
				}
				var mt *int64
				if c.rec.MTime != 0 {
					t := c.rec.MTime
					mt = &t
				}
				idmap[c.rec.ID] = s.Add(c.rec.Name, "dir", idmap[h], 0, nil, !c.rec.Live, mt, "fs", "")
				queue = append(queue, c.rec.ID)
			} else {
				addFile(idmap[h], c.rec)
			}
		}
	}
	orphans := -1 // entries whose parent folder record is gone (typical after deletion)
	for _, f := range cat.Files {
		if _, ok := idmap[f.Parent]; ok || hiddenIDs[f.Parent] || hidden[f.Name] {
			continue
		}
		if orphans < 0 {
			orphans = s.Add("Orphaned files (parent folder lost)", "dir", root, 0, nil, false, nil, "fs", "")
		}
		addFile(orphans, f)
	}
	return nil
}

func sortChildren[T any](list []T, key func(i int) (bool, string)) {
	type k struct {
		dir  bool
		name string
	}
	keys := make([]k, len(list))
	for i := range list {
		d, n := key(i)
		keys[i] = k{d, strings.ToLower(n)}
	}
	idx := make([]int, len(list))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ka, kb := keys[idx[a]], keys[idx[b]]
		if ka.dir != kb.dir {
			return ka.dir
		}
		return ka.name < kb.name
	})
	out := make([]T, len(list))
	for i, j := range idx {
		out[i] = list[j]
	}
	copy(list, out)
}
