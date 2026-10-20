// Package carve finds files by their content ("deep scan"), without any
// filesystem metadata. Each carver validates the file's internal structure
// and returns its exact length, so random byte matches are rejected.
package carve

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

var (
	le = binary.LittleEndian
	be = binary.BigEndian
)

const maxFile = 256 << 20

type carverFn func(r *rawdisk.Reader, s int64) int64
type kindFn func(r *rawdisk.Reader, s int64) string

// Carver describes one file signature.
type Carver struct {
	Needle []byte
	Rel    int64 // offset of the needle inside the file
	Group  string
	Ext    string
	Kind   kindFn // optional: refines Ext from the content
	Fn     carverFn
}

// Carvers is the list of supported file types.
var Carvers = []Carver{
	{[]byte("\x89PNG\r\n\x1a\n"), 0, "Pictures", "png", nil, carvePNG},
	{[]byte{0xff, 0xd8, 0xff}, 0, "Pictures", "jpg", nil, carveJPG},
	{[]byte("GIF89a"), 0, "Pictures", "gif", nil, carveGIF},
	{[]byte("GIF87a"), 0, "Pictures", "gif", nil, carveGIF},
	{[]byte("BM"), 0, "Pictures", "bmp", nil, carveBMP},
	{[]byte("%PDF-"), 0, "Documents", "pdf", nil, carvePDF},
	{[]byte("PK\x03\x04"), 0, "Archives", "zip", zipKind, carveZIP},
	{[]byte("SQLite format 3\x00"), 0, "Documents", "sqlite", nil, carveSQLite},
	{[]byte("ftyp"), 4, "Video", "mp4", mp4Kind, carveMP4},
	{[]byte("RIFF"), 0, "Audio", "riff", riffKind, carveRIFF},
}

// Hit is one carved file.
type Hit struct {
	Offset int64
	Length int64
	Group  string
	Ext    string
}

// Scan carves [start, end). emit is called for every validated file;
// onBytes reports progress; check returns an error to cancel.
func Scan(r *rawdisk.Reader, start, end int64, check func() error, emit func(Hit), onBytes func(int64)) error {
	const chunk = 8 << 20
	if end > r.Size {
		end = r.Size
	}
	covered := start // skip hits inside a file already carved
	type hit struct {
		pos int64
		c   *Carver
	}
	for off := start - start%rawdisk.Align; off < end; off += chunk {
		if err := check(); err != nil {
			return err
		}
		want := int64(chunk)
		if end-off < want {
			want = end - off
		}
		want += (rawdisk.Sector - want%rawdisk.Sector) % rawdisk.Sector
		data, _ := r.ReadRaw(off, int(want)+rawdisk.Align)
		var hits []hit
		for ci := range Carvers {
			c := &Carvers[ci]
			for i := 0; i < len(data) && i < chunk; {
				k := bytes.Index(data[i:], c.Needle)
				if k < 0 || i+k >= chunk {
					break
				}
				hits = append(hits, hit{off + int64(i+k) - c.Rel, c})
				i += k + 1
			}
		}
		sort.Slice(hits, func(a, b int) bool { return hits[a].pos < hits[b].pos })
		for _, h := range hits {
			if h.pos < covered || h.pos < start || h.pos >= end {
				continue
			}
			if h.c.Ext == "bmp" && h.pos%rawdisk.Sector != 0 {
				continue
			}
			ln := safe(h.c.Fn, r, h.pos)
			if ln < 32 || h.pos+ln > r.Size {
				continue
			}
			ext := h.c.Ext
			if h.c.Kind != nil {
				ext = h.c.Kind(r, h.pos)
			}
			emit(Hit{h.pos, ln, h.c.Group, ext})
			covered = h.pos + ln
		}
		step := int64(chunk)
		if end-off < step {
			step = end - off
		}
		onBytes(step)
	}
	return nil
}

func safe(fn carverFn, r *rawdisk.Reader, s int64) (n int64) {
	defer func() {
		if recover() != nil {
			n = 0
		}
	}()
	return fn(r, s)
}

func isAlpha(b []byte) bool {
	for _, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func carvePNG(r *rawdisk.Reader, s int64) int64 {
	for p := s + 8; p-s < maxFile; {
		h := r.MustRead(p, 8)
		if len(h) < 8 {
			return 0
		}
		ln := int64(be.Uint32(h))
		if !isAlpha(h[4:8]) || ln > 0x7fffffff {
			return 0
		}
		p += 12 + ln
		if string(h[4:8]) == "IEND" {
			return p - s
		}
	}
	return 0
}

func carveJPG(r *rawdisk.Reader, s int64) int64 {
	p := s + 2
	for { // header segments
		m := r.MustRead(p, 4)
		if len(m) < 4 || m[0] != 0xff {
			return 0
		}
		mk := m[1]
		if mk == 0xd8 || (mk >= 0xd0 && mk <= 0xd7) || mk == 0x01 {
			p += 2
			continue
		}
		ln := int64(be.Uint16(m[2:]))
		if ln < 2 {
			return 0
		}
		p += 2 + ln
		if mk == 0xda { // start of scan: entropy-coded data follows
			break
		}
		if p-s > maxFile {
			return 0
		}
	}
	for p-s < maxFile {
		blk := r.MustRead(p, 1<<16)
		if len(blk) == 0 {
			return 0
		}
		if i := bytes.Index(blk, []byte{0xff, 0xd9}); i >= 0 {
			return p + int64(i) + 2 - s
		}
		p += int64(len(blk)) - 1
	}
	return 0
}

func carveGIF(r *rawdisk.Reader, s int64) int64 {
	h := r.MustRead(s, 13)
	if len(h) < 13 {
		return 0
	}
	p := s + 13
	if h[10]&0x80 != 0 {
		p += 3 * (2 << (h[10] & 7))
	}
	for p-s < maxFile {
		t := r.MustRead(p, 1)
		if len(t) == 0 {
			return 0
		}
		switch t[0] {
		case ';':
			return p + 1 - s
		case '!':
			p += 2
		case ',':
			d := r.MustRead(p, 10)
			if len(d) < 10 {
				return 0
			}
			p += 10
			if d[9]&0x80 != 0 {
				p += 3 * (2 << (d[9] & 7))
			}
			p++
		default:
			return 0
		}
		for { // data sub-blocks
			n := r.MustRead(p, 1)
			if len(n) == 0 {
				return 0
			}
			p += 1 + int64(n[0])
			if n[0] == 0 {
				break
			}
		}
	}
	return 0
}

func carvePDF(r *rawdisk.Reader, s int64) int64 {
	var end int64
	for p := s; p-s < maxFile; {
		blk := r.MustRead(p, 1<<20)
		if len(blk) == 0 {
			break
		}
		i := bytes.Index(blk, []byte("%%EOF"))
		if i < 0 {
			if end != 0 {
				break
			}
			p += int64(len(blk)) - 4
			continue
		}
		end = p + int64(i) + 5
		tail := r.MustRead(end, 4096)
		if !bytes.Contains(tail, []byte("obj")) && !bytes.Contains(tail, []byte("xref")) {
			break // no incremental update follows
		}
		p = end
	}
	if end == 0 {
		return 0
	}
	return end - s
}

func carveZIP(r *rawdisk.Reader, s int64) int64 {
	for p := s; p-s < maxFile; {
		blk := r.MustRead(p, 1<<20)
		if len(blk) == 0 {
			return 0
		}
		if i := bytes.Index(blk, []byte("PK\x05\x06")); i >= 0 {
			e := r.MustRead(p+int64(i), 22)
			if len(e) < 22 {
				return 0
			}
			return p + int64(i) + 22 + int64(le.Uint16(e[20:])) - s
		}
		p += int64(len(blk)) - 3
	}
	return 0
}

var mp4Boxes = map[string]bool{"ftyp": true, "moov": true, "mdat": true, "free": true, "skip": true,
	"wide": true, "uuid": true, "meta": true, "pdin": true, "moof": true, "mfra": true, "sidx": true,
	"pnot": true, "PICT": true}

func carveMP4(r *rawdisk.Reader, s int64) int64 {
	p := s
	seen := map[string]bool{}
	for p-s < 8<<30 {
		h := r.MustRead(p, 16)
		if len(h) < 8 || !mp4Boxes[string(h[4:8])] {
			break
		}
		ln := int64(be.Uint32(h))
		if ln == 1 && len(h) >= 16 {
			ln = int64(be.Uint64(h[8:]))
		}
		if ln < 8 {
			break
		}
		seen[string(h[4:8])] = true
		p += ln
	}
	if seen["moov"] || seen["mdat"] {
		return p - s
	}
	return 0
}

func carveBMP(r *rawdisk.Reader, s int64) int64 {
	h := r.MustRead(s, 30)
	if len(h) < 30 {
		return 0
	}
	size, res, off, hdr := int64(le.Uint32(h[2:])), le.Uint32(h[6:]), int64(le.Uint32(h[10:])), le.Uint32(h[14:])
	if res != 0 || !(hdr == 12 || hdr == 40 || hdr == 56 || hdr == 108 || hdr == 124) || !(off >= 26 && off < size && size < maxFile) {
		return 0
	}
	return size
}

func carveRIFF(r *rawdisk.Reader, s int64) int64 {
	h := r.MustRead(s, 12)
	if len(h) < 12 {
		return 0
	}
	switch string(h[8:12]) {
	case "WAVE", "AVI ", "WEBP":
	default:
		return 0
	}
	ln := int64(le.Uint32(h[4:])) + 8
	if ln > 12 && ln < 4<<30 {
		return ln
	}
	return 0
}

func carveSQLite(r *rawdisk.Reader, s int64) int64 {
	h := r.MustRead(s, 100)
	if len(h) < 100 {
		return 0
	}
	ps := int64(be.Uint16(h[16:]))
	if ps == 1 {
		ps = 65536
	}
	cnt := int64(be.Uint32(h[28:]))
	if ps < 512 || ps&(ps-1) != 0 || cnt == 0 {
		return 0
	}
	return ps * cnt
}

func zipKind(r *rawdisk.Reader, s int64) string {
	nl := r.MustRead(s+26, 2)
	if len(nl) < 2 {
		return "zip"
	}
	name := string(r.MustRead(s+30, int(le.Uint16(nl))))
	switch name {
	case "[Content_Types].xml":
		body := r.MustRead(s, 1<<16)
		for _, k := range []struct{ dir, ext string }{{"word/", "docx"}, {"xl/", "xlsx"}, {"ppt/", "pptx"}} {
			if bytes.Contains(body, []byte(k.dir)) {
				return k.ext
			}
		}
		return "docx"
	case "mimetype":
		return "odt"
	}
	return "zip"
}

func riffKind(r *rawdisk.Reader, s int64) string {
	switch string(r.MustRead(s+8, 4)) {
	case "WAVE":
		return "wav"
	case "AVI ":
		return "avi"
	case "WEBP":
		return "webp"
	}
	return "riff"
}

func mp4Kind(r *rawdisk.Reader, s int64) string {
	switch string(r.MustRead(s+8, 4)) {
	case "qt  ":
		return "mov"
	case "heic", "mif1":
		return "heic"
	}
	return "mp4"
}
