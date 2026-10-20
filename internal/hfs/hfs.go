// Package hfs reads HFS+ and HFSX volumes directly, without the OS driver,
// following Apple Technical Note TN1150 ("HFS Plus Volume Format").
//
// It lists the catalog (names, folders, dates, extents), resolves extents
// overflow and hard links, reads the allocation bitmap to find free space,
// and recovers records of deleted files from stale catalog nodes, from the
// slack space inside live nodes, and from old node copies in the journal.
// When the catalog B-tree header node is damaged (a common reason macOS
// refuses to mount a volume) it walks every node instead.
package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"

	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

var be = binary.BigEndian

// Epoch converts HFS+ timestamps (seconds since 1904) to Unix time.
const Epoch = 2082844800

// Extent is a run of allocation blocks.
type Extent struct{ Start, Count uint32 }

// Volume is an opened HFS+ volume.
type Volume struct {
	r           *rawdisk.Reader
	Base        int64
	Kind        string
	BlockSize   int64
	TotalBlocks int64
	vh          []byte
	overflow    map[[2]uint32]map[uint32][]Extent // (fileID, forkType) -> startBlock -> extents
	bitmap      *Fork

	HeaderDamaged bool
	nodeSize      int
}

// Open reads the volume header at base+1024.
func Open(r *rawdisk.Reader, base int64) (*Volume, error) {
	vh := r.MustRead(base+1024, 512)
	if len(vh) < 512 || !(string(vh[:2]) == "H+" || string(vh[:2]) == "HX") {
		return nil, errors.New("no HFS+ volume header")
	}
	v := &Volume{r: r, Base: base, vh: vh, Kind: "HFS+", overflow: map[[2]uint32]map[uint32][]Extent{}}
	if vh[1] == 'X' {
		v.Kind = "HFSX"
	}
	v.BlockSize, v.TotalBlocks = int64(be.Uint32(vh[40:])), int64(be.Uint32(vh[44:]))
	if v.BlockSize < 512 || v.BlockSize&(v.BlockSize-1) != 0 {
		return nil, errors.New("invalid HFS+ block size")
	}
	return v, nil
}

func forkDesc(b []byte, off int) (int64, []Extent) {
	size := int64(be.Uint64(b[off:]))
	var ext []Extent
	for i := 0; i < 8; i++ {
		e := Extent{be.Uint32(b[off+16+8*i:]), be.Uint32(b[off+20+8*i:])}
		if e.Count != 0 {
			ext = append(ext, e)
		}
	}
	return size, ext
}

// Ranges turns extents into absolute byte ranges trimmed to size.
func (v *Volume) Ranges(ext []Extent, size int64) [][2]int64 {
	out := [][2]int64{}
	left := size
	for _, e := range ext {
		if left <= 0 {
			break
		}
		ln := int64(e.Count) * v.BlockSize
		if ln > left {
			ln = left
		}
		out = append(out, [2]int64{v.Base + int64(e.Start)*v.BlockSize, ln})
		left -= ln
	}
	return out
}

// ExtentsFor appends extents from the overflow file to a fork's first eight.
func (v *Volume) ExtentsFor(fileID uint32, base []Extent) []Extent {
	more := v.overflow[[2]uint32{fileID, 0}]
	if len(more) == 0 {
		return base
	}
	keys := make([]uint32, 0, len(more))
	for k := range more {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
	out := append([]Extent{}, base...)
	for _, k := range keys {
		out = append(out, more[k]...)
	}
	return out
}

// Fork reads a fork's logical bytes through its extent list.
type Fork struct {
	r      *rawdisk.Reader
	ranges [][2]int64
	Size   int64
}

func (v *Volume) fork(ext []Extent, size int64) *Fork {
	f := &Fork{r: v.r, ranges: v.Ranges(ext, size)}
	for _, x := range f.ranges {
		f.Size += x[1]
	}
	return f
}

// Read returns length bytes at logical offset off.
func (f *Fork) Read(off int64, length int) []byte {
	var out []byte
	var pos int64
	for _, rg := range f.ranges {
		a, l := rg[0], rg[1]
		if off < pos+l && length > 0 {
			s := off
			if s < pos {
				s = pos
			}
			e := pos + l
			if off+int64(length) < e {
				e = off + int64(length)
			}
			out = append(out, f.r.MustRead(a+s-pos, int(e-s))...)
			length -= int(e - s)
			off = e
		}
		pos += l
	}
	return out
}

// ---------------------------------------------------------------- B-tree

func looksLikeNode(node []byte) bool {
	if len(node) < 512 {
		return false
	}
	switch node[8] {
	case 0xff, 0, 1, 2:
	default:
		return false
	}
	cnt := int(be.Uint16(node[10:]))
	return cnt > 0 && cnt < len(node)/8 && be.Uint16(node[len(node)-2:]) == 14
}

func guessNodeSize(f *Fork) int {
	best, bestHits := 0, 0
	for _, ns := range []int{4096, 8192, 16384, 32768, 2048, 1024, 512} {
		n := int(f.Size / int64(ns))
		if n > 64 {
			n = 64
		}
		hits := 0
		for i := 1; i < n; i++ {
			if looksLikeNode(f.Read(int64(i*ns), ns)) {
				hits++
			}
		}
		if hits > bestHits {
			best, bestHits = ns, hits
		}
	}
	return best
}

type leaf struct {
	data []byte
	live bool
}

// leaves calls fn for every leaf node of the tree stored in f.
func (v *Volume) leaves(f *Fork, progress func(float64), fn func(leaf)) error {
	head := f.Read(0, 512)
	var ns, total int
	if len(head) >= 46 {
		ns, total = int(be.Uint16(head[32:])), int(be.Uint32(head[36:]))
	}
	damaged := len(head) < 46 || head[8] != 1 || ns < 512 || ns&(ns-1) != 0 || total == 0 ||
		int64(total)*int64(ns) > f.Size+int64(ns)
	var live map[int]bool
	if damaged {
		ns = guessNodeSize(f)
		if ns == 0 {
			return errors.New("B-tree is unreadable (no valid nodes found)")
		}
		total = int(f.Size / int64(ns))
	} else {
		live = map[int]bool{}
		for n := int(be.Uint32(head[24:])); n != 0 && !live[n] && n < total; {
			live[n] = true
			d := f.Read(int64(n*ns), 14)
			if len(d) < 14 {
				break
			}
			n = int(be.Uint32(d))
		}
	}
	v.nodeSize = ns
	for n := 1; n < total; n++ {
		if n%512 == 0 && progress != nil {
			progress(float64(n) / float64(total))
		}
		node := f.Read(int64(n*ns), ns)
		if len(node) < ns || node[8] != 0xff || node[9] != 1 {
			continue
		}
		isLive := live != nil && live[n]
		if live == nil {
			isLive = looksLikeNode(node)
		}
		fn(leaf{node, isLive})
	}
	if damaged {
		v.HeaderDamaged = true
	}
	return nil
}

func records(node []byte) []int {
	ns := len(node)
	cnt := int(be.Uint16(node[10:]))
	if cnt == 0 || cnt > ns/8 {
		return nil
	}
	var out []int
	for i := 0; i < cnt; i++ {
		off := int(be.Uint16(node[ns-2*(i+1):]))
		if off >= 14 && off < ns-8 {
			out = append(out, off)
		}
	}
	return out
}

// staleRecords brute-forces catalog file/folder records anywhere in buf.
// Catalog keys satisfy keyLength == 6 + 2*nameLength, a strong check.
func staleRecords(buf []byte, start int) []int {
	var out []int
	n := len(buf)
	for i := start + start&1; i < n-100; i += 2 {
		klen := int(buf[i])<<8 | int(buf[i+1])
		if klen < 8 || klen > 516 || klen&1 != 0 {
			continue
		}
		nlen := int(buf[i+6])<<8 | int(buf[i+7])
		if klen != 6+2*nlen || i+2+klen+4 > n {
			continue
		}
		d := i + 2 + klen
		if buf[d] == 0 && (buf[d+1] == 1 || buf[d+1] == 2) {
			out = append(out, i)
		}
	}
	return out
}

// LoadExtentsOverflow reads the extents overflow file (file ID 3).
func (v *Volume) LoadExtentsOverflow(progress func(float64)) error {
	size, ext := forkDesc(v.vh, 192)
	return v.leaves(v.fork(ext, size), progress, func(l leaf) {
		node := l.data
		for _, off := range records(node) {
			if off+12+64 > len(node) || be.Uint16(node[off:]) != 10 {
				continue
			}
			ftype, fid, start := node[off+2], be.Uint32(node[off+4:]), be.Uint32(node[off+8:])
			d := off + 12
			var exts []Extent
			for i := 0; i < 8; i++ {
				e := Extent{be.Uint32(node[d+8*i:]), be.Uint32(node[d+4+8*i:])}
				if e.Count != 0 {
					exts = append(exts, e)
				}
			}
			key := [2]uint32{fid, uint32(ftype)}
			if v.overflow[key] == nil {
				v.overflow[key] = map[uint32][]Extent{}
			}
			if _, ok := v.overflow[key][start]; !ok {
				v.overflow[key][start] = exts
			}
		}
	})
}

// IsAllocated reports whether an allocation block is in use.
func (v *Volume) IsAllocated(block int64) bool {
	if v.bitmap == nil {
		size, ext := forkDesc(v.vh, 112)
		v.bitmap = v.fork(ext, size)
	}
	b := v.bitmap.Read(block/8, 1)
	return len(b) == 1 && b[0]&(0x80>>(block%8)) != 0
}

// FreeRuns returns unallocated regions as absolute byte ranges, exact to the
// block, read from the allocation bitmap.
func (v *Volume) FreeRuns() [][2]int64 {
	size, ext := forkDesc(v.vh, 112)
	bm := v.fork(ext, size)
	nbytes := (v.TotalBlocks + 7) / 8
	var runs [][2]int64 // [startBlock, count]
	push := func(blk, cnt int64) {
		if k := len(runs) - 1; k >= 0 && runs[k][0]+runs[k][1] == blk {
			runs[k][1] += cnt
		} else {
			runs = append(runs, [2]int64{blk, cnt})
		}
	}
	const chunk = 8 << 20
	for base := int64(0); base < nbytes; base += chunk {
		n := int64(chunk)
		if nbytes-base < n {
			n = nbytes - base
		}
		data := bm.Read(base, int(n))
		for i, c := range data {
			blk := (base + int64(i)) * 8
			switch c {
			case 0xff:
			case 0:
				push(blk, 8)
			default:
				for bit := int64(0); bit < 8; bit++ {
					if c&(0x80>>bit) == 0 {
						push(blk+bit, 1)
					}
				}
			}
		}
	}
	var out [][2]int64
	for _, rg := range runs {
		cnt := rg[1]
		if rg[0]+cnt > v.TotalBlocks {
			cnt = v.TotalBlocks - rg[0]
		}
		if cnt > 0 {
			out = append(out, [2]int64{v.Base + rg[0]*v.BlockSize, cnt * v.BlockSize})
		}
	}
	return out
}

func (v *Volume) journalRegion() (int64, int64, bool) {
	jib := int64(be.Uint32(v.vh[12:]))
	if be.Uint32(v.vh[4:])&0x2000 == 0 || jib == 0 {
		return 0, 0, false
	}
	j := v.r.MustRead(v.Base+jib*v.BlockSize, 52)
	if len(j) < 52 {
		return 0, 0, false
	}
	off, size := int64(be.Uint64(j[36:])), int64(be.Uint64(j[44:]))
	if size == 0 || off+size > v.TotalBlocks*v.BlockSize {
		return 0, 0, false
	}
	if size > 1<<30 {
		size = 1 << 30
	}
	return v.Base + off, size, true
}

// ---------------------------------------------------------------- catalog

// Record is a catalog file or folder record.
type Record struct {
	ID      uint32
	Parent  uint32
	Name    string
	Live    bool
	MTime   int64 // Unix seconds, 0 if unknown
	Size    int64
	Ext     []Extent
	Special uint32 // iNode number for hard links
	Flags   byte
	Type    string // Finder type+creator, e.g. "hlnkhfs+"
}

// Catalog holds every folder and file record found.
type Catalog struct {
	Folders map[uint32]*Record
	Files   map[uint32]*Record
}

func keep(table map[uint32]*Record, rec *Record) {
	old := table[rec.ID]
	if old == nil || (rec.Live && !old.Live) || (rec.Live == old.Live && rec.MTime > old.MTime) {
		table[rec.ID] = rec
	}
}

func decodeName(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = be.Uint16(b[2*i:])
	}
	return norm.NFC.String(string(utf16.Decode(u)))
}

func hfsTime(t uint32) int64 {
	if t == 0 {
		return 0
	}
	return int64(t) - Epoch
}

func (v *Volume) parse(node []byte, off int, live bool, cat *Catalog) {
	if off+8 > len(node) {
		return
	}
	klen, parent, nlen := int(be.Uint16(node[off:])), be.Uint32(node[off+2:]), int(be.Uint16(node[off+6:]))
	if klen < 6 || klen > 516 || 8+2*nlen > klen+2 || (!live && klen != 6+2*nlen) {
		return
	}
	d := off + 2 + klen
	if d+2 > len(node) {
		return
	}
	name := decodeName(node[off+8 : off+8+2*nlen])
	switch int16(be.Uint16(node[d:])) {
	case 1:
		if d+20 > len(node) {
			return
		}
		keep(cat.Folders, &Record{ID: be.Uint32(node[d+8:]), Parent: parent, Name: name, Live: live,
			MTime: hfsTime(be.Uint32(node[d+16:]))})
	case 2:
		if d+168 > len(node) {
			return
		}
		size, ext := forkDesc(node, d+88)
		if !live {
			var blocks int64
			for _, e := range ext {
				blocks += int64(e.Count)
				if int64(e.Start)+int64(e.Count) > v.TotalBlocks {
					return
				}
			}
			if size > blocks*v.BlockSize {
				return
			}
		}
		keep(cat.Files, &Record{ID: be.Uint32(node[d+8:]), Parent: parent, Name: name, Live: live,
			MTime: hfsTime(be.Uint32(node[d+16:])), Size: size, Ext: ext, Special: be.Uint32(node[d+44:]),
			Flags: node[d+41], Type: string(node[d+48 : d+56])})
	}
}

// ReadCatalog reads the catalog B-tree (file ID 4) and the journal.
func (v *Volume) ReadCatalog(progress func(float64)) (*Catalog, error) {
	size, ext := forkDesc(v.vh, 272)
	f := v.fork(v.ExtentsFor(4, ext), size)
	cat := &Catalog{Folders: map[uint32]*Record{}, Files: map[uint32]*Record{}}
	visit := func(node []byte, live bool) {
		if live {
			cnt := int(be.Uint16(node[10:]))
			free := len(node)
			if cnt < len(node)/8 {
				free = int(be.Uint16(node[len(node)-2*(cnt+1):]))
			}
			for _, off := range records(node) {
				v.parse(node, off, true, cat)
			}
			for _, off := range staleRecords(node, free) { // slack after the live records
				v.parse(node, off, false, cat)
			}
			return
		}
		for _, off := range staleRecords(node, 0) {
			v.parse(node, off, false, cat)
		}
	}
	if err := v.leaves(f, progress, func(l leaf) { visit(l.data, l.live) }); err != nil {
		return nil, err
	}
	if off, size, ok := v.journalRegion(); ok && v.nodeSize > 0 { // old node copies in the journal
		const chunk = 8 << 20
		for p := int64(0); p < size; p += chunk {
			n := int64(chunk)
			if size-p < n {
				n = size - p
			}
			data := v.r.MustRead(off+p, int(n))
			for i := 0; ; {
				k := bytes.Index(data[i:], []byte{0xff, 0x01})
				if k < 0 {
					break
				}
				i += k
				if i%512 == 8 && i-8+v.nodeSize <= len(data) {
					visit(data[i-8:i-8+v.nodeSize], false)
				}
				i++
			}
		}
	}
	return cat, nil
}
