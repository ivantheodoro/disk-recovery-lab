package detect

import (
	"bytes"
	"fmt"
	"os"

	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

// Job is the progress/cancel hook used by long operations.
type Job interface {
	Check() error
	Log(string)
	SetProgress(float64)
}

// Found is a boot sector or superblock located by the lost-partition search.
type Found struct {
	Offset   int64  `json:"offset"`
	FS       *FS    `json:"fs"`
	BackupOf *int64 `json:"backup_of,omitempty"`
	Note     string `json:"note,omitempty"`
}

var lostSigs = []struct {
	needle []byte
	rel    int64
}{
	{[]byte("NTFS    "), 3}, {[]byte("EXFAT   "), 3}, {[]byte("FAT32   "), 82}, {[]byte("NXSB"), 32},
	{[]byte("H+\x00\x04"), 1024}, {[]byte("HX\x00\x05"), 1024}, {[]byte("-FVE-FS-"), 3},
	{[]byte{0x53, 0xef}, 1080}, // ext2/3/4 magic at partition+0x438
}

// SearchLostPartitions scans the whole source for volume headers, so wiped
// or overwritten partition tables can be rebuilt.
func SearchLostPartitions(path string, job Job) ([]Found, error) {
	r, err := rawdisk.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var found []Found
	seen := map[int64]bool{}
	const chunk = 8 << 20
	for off := int64(0); off < r.Size; off += chunk {
		if err := job.Check(); err != nil {
			return found, err
		}
		data, err := r.ReadRaw(off, chunk+2048)
		if err != nil {
			job.Log(fmt.Sprintf("Read error at %d: %v (skipped)", off, err))
			continue
		}
		for _, s := range lostSigs {
			for i := 0; ; {
				k := bytes.Index(data[i:], s.needle)
				if k < 0 {
					break
				}
				i += k
				start := off + int64(i) - s.rel
				i++
				if start < 0 || start%rawdisk.Sector != 0 || seen[start] {
					continue
				}
				fs := DetectFS(r, start)
				if fs == nil || (s.rel == 1080 && fs.Name[:3] != "ext") {
					continue
				}
				seen[start] = true
				found = append(found, Found{Offset: start, FS: fs})
				job.Log(fmt.Sprintf("Found %s at offset %d (%s)", fs.Name, start, fs.Detail))
			}
		}
		job.SetProgress(float64(off+chunk) / float64(r.Size))
	}
	for i := range found {
		annotateBackup(r, &found[i])
	}
	return found, nil
}

// FAT32, exFAT and NTFS keep a backup boot sector. When the primary was
// wiped, the search only finds the backup: work out the real start.
func annotateBackup(r *rawdisk.Reader, f *Found) {
	b := r.MustRead(f.Offset, 512)
	if len(b) < 512 {
		return
	}
	bps := int64(le.Uint16(b[11:]))
	if bps == 0 {
		bps = 512
	}
	var dist int64
	switch f.FS.Name {
	case "FAT32":
		dist = int64(le.Uint16(b[50:])) * bps
	case "exFAT":
		dist = 12 * (int64(1) << b[108])
	case "NTFS":
		dist = int64(le.Uint64(b[40:])) * bps
	default:
		return
	}
	cand := f.Offset - dist
	if dist == 0 || cand < 0 {
		return
	}
	if prim := DetectFS(r, cand); prim == nil || prim.Name != f.FS.Name {
		f.BackupOf = &cand
		f.Note = fmt.Sprintf("This is the BACKUP boot sector. The primary at offset %d is damaged; the "+
			"partition really starts there.", cand)
	}
}

// ImageResult describes a byte-to-byte image.
type ImageResult struct {
	Image      string         `json:"image"`
	Size       int64          `json:"size"`
	BadSectors int            `json:"bad_sectors"`
	BadList    []int64        `json:"bad_sector_list"`
	Repaired   map[string]any `json:"repaired,omitempty"`
}

// MakeImage copies every sector of src to dst. Unreadable sectors are
// zero-filled and listed, so a failing drive only has to be read once.
func MakeImage(src, dst string, job Job) (*ImageResult, error) {
	r, err := rawdisk.Open(src)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	res := &ImageResult{Image: dst, Size: r.Size, BadList: []int64{}}
	const chunk = 1 << 20
	for off := int64(0); off < r.Size; off += chunk {
		if err := job.Check(); err != nil {
			return res, err
		}
		n := int64(chunk)
		if r.Size-off < n {
			n = r.Size - off
		}
		data, err := r.ReadRaw(off, int(n))
		if err != nil || int64(len(data)) < n {
			data = make([]byte, 0, n)
			for s := off; s < off+n; s += rawdisk.Sector {
				b, e := r.ReadRaw(s, rawdisk.Sector)
				if e != nil || len(b) < rawdisk.Sector {
					res.BadSectors++
					if len(res.BadList) < 1000 {
						res.BadList = append(res.BadList, s/rawdisk.Sector)
					}
					b = make([]byte, rawdisk.Sector)
				}
				data = append(data, b...)
			}
			job.Log(fmt.Sprintf("Read error near offset %d; %d bad sector(s) zero-filled so far", off, res.BadSectors))
		}
		if _, err := out.Write(data); err != nil {
			return res, err
		}
		job.SetProgress(float64(off+n) / float64(r.Size))
	}
	return res, out.Sync()
}

// RepairedCopy images src to dst, then restores the damaged primary boot
// region from its backup inside the COPY. The source is never written.
func RepairedCopy(src, dst string, partStart, backupOff int64, kind string, job Job) (*ImageResult, error) {
	res, err := MakeImage(src, dst, job)
	if err != nil {
		return res, err
	}
	r, err := rawdisk.Open(src)
	if err != nil {
		return res, err
	}
	n := 3 * 512 // FAT32: boot sector, FSInfo and the third reserved sector
	switch kind {
	case "exFAT":
		n = int(backupOff - partStart)
	case "NTFS":
		n = 512
	}
	region := r.MustRead(backupOff, n)
	r.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return res, err
	}
	defer f.Close()
	if _, err := f.WriteAt(region, partStart); err != nil {
		return res, err
	}
	job.Log(fmt.Sprintf("Restored %d bytes of %s boot region at offset %d from backup at %d", len(region), kind, partStart, backupOff))
	res.Repaired = map[string]any{"offset": partStart, "from": backupOff, "fs": kind}
	return res, nil
}
