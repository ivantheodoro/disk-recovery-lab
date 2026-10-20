// Package detect recognises partition tables and filesystems from their
// on-disk signatures, explains why an OS may refuse to mount a volume,
// searches for lost partitions, and makes byte-to-byte images.
package detect

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode/utf16"

	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

var (
	le = binary.LittleEndian
	be = binary.BigEndian
)

// FS describes a recognised filesystem.
type FS struct {
	Name   string `json:"fs"`
	Native string `json:"native"` // support on macOS: read-write, read-only, no
	Detail string `json:"detail"`
}

// Partition is one entry of an MBR or GPT partition table.
type Partition struct {
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	FS     *FS    `json:"fs"`
}

// Probe is the result of diagnosing a source.
type Probe struct {
	Path        string      `json:"path"`
	Size        int64       `json:"size"`
	Scheme      string      `json:"scheme"`
	WholeDiskFS *FS         `json:"whole_disk_fs"`
	Partitions  []Partition `json:"partitions"`
	Sector0Hex  string      `json:"first_sector_hex"`
	Sector0Zero bool        `json:"first_sector_blank"`
	Verdict     []string    `json:"verdict"`
}

// DetectFS checks the boot sector / superblock at base.
func DetectFS(r *rawdisk.Reader, base int64) *FS {
	b := r.MustRead(base, 0x10100)
	if len(b) < 1100 {
		return nil
	}
	at := func(off int, s string) bool { return len(b) >= off+len(s) && string(b[off:off+len(s)]) == s }
	switch {
	case at(3, "NTFS    "):
		bps := int64(le.Uint16(b[11:]))
		return &FS{"NTFS", "read-only", fmt.Sprintf("%s, %d B/sector", Human(int64(le.Uint64(b[40:]))*bps), bps)}
	case at(3, "EXFAT   "):
		return &FS{"exFAT", "read-write", fmt.Sprintf("%s, %d B/sector", Human(int64(le.Uint64(b[72:]))<<b[108]), 1<<b[108])}
	case at(3, "-FVE-FS-"):
		return &FS{"BitLocker", "no", "Encrypted Windows volume"}
	case at(3, "ReFS\x00\x00\x00\x00"):
		return &FS{"ReFS", "no", "Windows Resilient FS"}
	case at(82, "FAT32   "):
		return &FS{"FAT32", "read-write", fatInfo(b)}
	case at(54, "FAT12") || at(54, "FAT16"):
		return &FS{string(b[54:59]), "read-write", fatInfo(b)}
	case at(32, "NXSB"):
		bs, cnt := int64(le.Uint32(b[36:])), int64(le.Uint64(b[40:]))
		return &FS{"APFS", "read-write", fmt.Sprintf("block %d, %s", bs, Human(bs*cnt))}
	case at(1024, "H+") || at(1024, "HX"):
		bs, cnt := int64(be.Uint32(b[1024+40:])), int64(be.Uint32(b[1024+44:]))
		name := "HFS+"
		if b[1025] == 'X' {
			name = "HFSX"
		}
		return &FS{name, "read-write", fmt.Sprintf("block %d, %s", bs, Human(bs*cnt))}
	case b[1024+56] == 0x53 && b[1024+57] == 0xef:
		compat, incompat := le.Uint32(b[1024+92:]), le.Uint32(b[1024+96:])
		kind := "ext2"
		if incompat&0x40 != 0 {
			kind = "ext4"
		} else if compat&0x4 != 0 {
			kind = "ext3"
		}
		cnt, bs := int64(le.Uint32(b[1024+4:])), int64(1024)<<le.Uint32(b[1024+24:])
		label := string(bytes.TrimRight(b[1024+120:1024+136], "\x00"))
		return &FS{kind, "no", fmt.Sprintf("label '%s', %s", label, Human(bs*cnt))}
	case at(0x10040, "_BHRfS_M"):
		return &FS{"Btrfs", "no", "Linux Btrfs"}
	case at(0, "XFSB"):
		return &FS{"XFS", "no", "Linux XFS"}
	case at(0, "LUKS\xba\xbe"):
		return &FS{"LUKS", "no", "Linux encrypted container"}
	case at(0x8001, "CD001"):
		return &FS{"ISO9660", "read-only", "Optical image"}
	}
	return nil
}

func fatInfo(b []byte) string {
	bps, spc := le.Uint16(b[11:]), b[13]
	label := b[43:54]
	if string(b[82:87]) == "FAT32" {
		label = b[71:82]
	}
	return fmt.Sprintf("label '%s', %d B/sector, %d sectors/cluster", strings.TrimSpace(string(label)), bps, spc)
}

var guids = map[string]string{
	"C12A7328-F81F-11D2-BA4B-00A0C93EC93B": "EFI System",
	"7C3457EF-0000-11AA-AA11-00306543ECAC": "Apple APFS",
	"48465300-0000-11AA-AA11-00306543ECAC": "Apple HFS+",
	"EBD0A0A2-B9E5-4433-87C0-68B6B72699C7": "Microsoft Basic Data",
	"0FC63DAF-8483-4772-8E79-3D69D8477DE4": "Linux filesystem",
	"E3C9E316-0B5C-4DB8-817D-F92DF00215AE": "Microsoft Reserved",
	"DE94BBA4-06D1-4D40-A16A-BFD50179D6AC": "Windows Recovery",
}

func guid(raw []byte) string {
	s := fmt.Sprintf("%08X-%04X-%04X-%s-%s", le.Uint32(raw), le.Uint16(raw[4:]), le.Uint16(raw[6:]),
		strings.ToUpper(hex.EncodeToString(raw[8:10])), strings.ToUpper(hex.EncodeToString(raw[10:16])))
	if n, ok := guids[s]; ok {
		return n
	}
	return s
}

// PartitionTable parses GPT (512 or 4096-byte sectors) or MBR.
func PartitionTable(r *rawdisk.Reader) (string, []Partition) {
	for _, ss := range []int64{512, 4096} {
		hdr := r.MustRead(ss, 92)
		if len(hdr) < 92 || string(hdr[:8]) != "EFI PART" {
			continue
		}
		entLBA, n, esz := int64(le.Uint64(hdr[72:])), int(le.Uint32(hdr[80:])), int(le.Uint32(hdr[84:]))
		chk := append(append(append([]byte{}, hdr[:16]...), 0, 0, 0, 0), hdr[20:92]...)
		crcOK := crc32.ChecksumIEEE(chk) == le.Uint32(hdr[16:])
		if n > 256 {
			n = 256
		}
		if esz < 128 || esz > 4096 {
			esz = 128
		}
		tbl := r.MustRead(entLBA*ss, n*esz)
		var parts []Partition
		for i := 0; i < n && (i+1)*esz <= len(tbl); i++ {
			e := tbl[i*esz : (i+1)*esz]
			if bytes.Equal(e[:16], make([]byte, 16)) {
				continue
			}
			first, last := int64(le.Uint64(e[32:])), int64(le.Uint64(e[40:]))
			u := make([]uint16, 36)
			for k := range u {
				u[k] = le.Uint16(e[56+2*k:])
			}
			name := strings.TrimRight(string(utf16.Decode(u)), "\x00")
			parts = append(parts, Partition{Index: i + 1, Type: guid(e[:16]), Name: name,
				Offset: first * ss, Size: (last - first + 1) * ss})
		}
		scheme := "GPT"
		if !crcOK {
			scheme += " (header CRC mismatch)"
		}
		return scheme, parts
	}
	b := r.MustRead(0, 512)
	if len(b) == 512 && b[510] == 0x55 && b[511] == 0xaa && string(b[3:11]) != "NTFS    " &&
		string(b[3:11]) != "EXFAT   " && string(b[82:87]) != "FAT32" {
		var parts []Partition
		for i := 0; i < 4; i++ {
			e := b[446+16*i : 462+16*i]
			start, cnt := int64(le.Uint32(e[8:])), int64(le.Uint32(e[12:]))
			if e[4] != 0 && cnt != 0 {
				parts = append(parts, Partition{Index: i + 1, Type: fmt.Sprintf("0x%02x", e[4]),
					Offset: start * 512, Size: cnt * 512})
			}
		}
		return "MBR", parts
	}
	return "none", nil
}

// Diagnose reads the first sectors of a source and explains what it finds.
func Diagnose(path string) (*Probe, error) {
	r, err := rawdisk.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	scheme, parts := PartitionTable(r)
	for i := range parts {
		parts[i].FS = DetectFS(r, parts[i].Offset)
	}
	s0 := r.MustRead(0, 512)
	p := &Probe{Path: path, Size: r.Size, Scheme: scheme, WholeDiskFS: DetectFS(r, 0), Partitions: parts,
		Sector0Hex: hex.EncodeToString(s0), Sector0Zero: bytes.Equal(s0, make([]byte, len(s0)))}
	if p.Partitions == nil {
		p.Partitions = []Partition{}
	}
	p.Verdict = verdict(p)
	return p, nil
}

func verdict(p *Probe) []string {
	var msgs []string
	var fss []*FS
	for _, x := range p.Partitions {
		if x.FS != nil {
			fss = append(fss, x.FS)
		}
	}
	if p.WholeDiskFS != nil {
		fss = append(fss, p.WholeDiskFS)
	}
	if len(fss) == 0 {
		if p.Sector0Zero {
			msgs = append(msgs, "The first sector is all zeros: the partition table/boot sector is wiped or the "+
				"disk was never initialised. Use 'Find lost partitions', then scan.")
		} else {
			msgs = append(msgs, "No known partition table or filesystem signature was found. The metadata is "+
				"damaged or the disk is encrypted. Make a byte-to-byte image first, then scan the image.")
		}
	}
	for _, f := range fss {
		switch f.Native {
		case "no":
			msgs = append(msgs, f.Name+" cannot be mounted by macOS or Windows without extra drivers. Scan it: "+
				"this app reads it directly.")
		case "read-only":
			msgs = append(msgs, f.Name+" is supported read-only by macOS: try 'Mount read-only'.")
		default:
			msgs = append(msgs, f.Name+" is a supported filesystem. If it does not mount, its internal "+
				"structures are damaged: run 'Check filesystem', then scan it.")
		}
	}
	if strings.Contains(p.Scheme, "mismatch") {
		msgs = append(msgs, "The GPT header checksum is wrong: the partition map is corrupt, a common reason "+
			"for a disk to show as 'not readable'.")
	}
	return msgs
}

// Human formats a byte count with decimal units, like Finder.
func Human(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < 1000 || u == "TB" {
			if u == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1000
	}
	return ""
}
