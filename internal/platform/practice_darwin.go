//go:build darwin

package platform

import (
	"archive/zip"
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ivantheodoro/disk-recovery-lab/internal/plist"
)

// PracticeResult describes a generated practice disk.
type PracticeResult struct {
	Image   string   `json:"image"`
	Kept    []string `json:"kept"`
	Deleted []string `json:"deleted"`
	Damaged bool     `json:"damaged"`
}

func pngBytes(w, h int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func pdfBytes(text string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 30 100 Td (%s) Tj ET", text)
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offs []int
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

func wavBytes() []byte {
	const rate = 8000
	data := make([]byte, 2*rate)
	for i := 0; i < rate; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/rate))
		data[2*i], data[2*i+1] = byte(v), byte(v>>8)
	}
	h := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x80\x3e\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")
	put := func(o, v int) { h[o], h[o+1], h[o+2], h[o+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
	put(4, 36+len(data))
	put(40, len(data))
	return append(h, data...)
}

func zipBytes() []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("notes.txt")
	w.Write([]byte(strings.Repeat("This archive was deleted and found again.\n", 50)))
	z.Close()
	return b.Bytes()
}

func attachRaw(path string) (string, error) {
	m, err := plistOf("hdiutil", "attach", "-nomount", "-plist", "-imagekey", "diskimage-class=CRawDiskImage", path)
	if err != nil {
		return "", err
	}
	for _, e := range plist.Arr(m["system-entities"]) {
		if d := plist.Str(plist.Dict(e)["dev-entry"]); regexp.MustCompile(`^/dev/disk\d+$`).MatchString(d) {
			return d, nil
		}
	}
	return "", fmt.Errorf("hdiutil did not return a device")
}

// hfsPartition returns the HFS+ partition that eraseDisk created on dev
// (small disks get no EFI partition, so it is not always s2).
func hfsPartition(dev string) string {
	m, err := plistOf("diskutil", "list", "-plist", dev)
	if err == nil {
		for _, d := range plist.Arr(m["AllDisksAndPartitions"]) {
			for _, p := range plist.Arr(plist.Dict(d)["Partitions"]) {
				if pd := plist.Dict(p); strings.Contains(plist.Str(pd["Content"]), "HFS") {
					return "/dev/" + plist.Str(pd["DeviceIdentifier"])
				}
			}
		}
	}
	return dev + "s2"
}

func writeAll(root string, files map[string][]byte) error {
	for rel, data := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func remount(dev, mnt string) {
	run("sync")
	run("diskutil", "unmount", mnt)
	time.Sleep(300 * time.Millisecond)
	run("diskutil", "mount", "-mountPoint", mnt, dev)
}

// CreatePractice makes a small disk image with files deleted on purpose.
// kind is "mac" (HFS+ like an external Mac drive), "usb" (FAT32) or
// "damaged" (FAT32 with its boot sector wiped, so macOS cannot read it).
func CreatePractice(dir, kind string) (*PracticeResult, error) {
	os.MkdirAll(dir, 0o755)
	name := map[string]string{"mac": "practice-mac-drive.img", "usb": "practice-usb.img", "damaged": "practice-damaged-usb.img"}[kind]
	if name == "" {
		return nil, fmt.Errorf("unknown practice disk %q", kind)
	}
	path := filepath.Join(dir, name)
	os.Remove(path)
	size := int64(64 << 20)
	if kind == "mac" {
		size = 128 << 20
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	f.Truncate(size)
	f.Close()
	dev, err := attachRaw(path)
	if err != nil {
		return nil, err
	}
	defer run("hdiutil", "detach", "-force", dev)
	tmp, _ := os.MkdirTemp("", "drl-practice-")
	defer os.RemoveAll(tmp)
	mnt := filepath.Join(tmp, "mnt")
	os.Mkdir(mnt, 0o755)
	var keep, del map[string][]byte
	vol := dev
	if kind == "mac" {
		if out, err := run("diskutil", "eraseDisk", "JHFSX", "LABMAC", "GPT", dev); err != nil {
			return nil, fmt.Errorf("eraseDisk: %s", out)
		}
		run("diskutil", "unmountDisk", dev)
		vol = hfsPartition(dev)
		if out, err := run("diskutil", "mount", "-mountPoint", mnt, vol); err != nil {
			return nil, fmt.Errorf("mount: %s", out)
		}
		keep = map[string][]byte{
			"Virtual Machines/Ubuntu.vmwarevm/Ubuntu.vmx":  []byte(".encoding = \"UTF-8\"\nconfig.version = \"8\"\nmemsize = \"4096\"\n"),
			"Virtual Machines/Ubuntu.vmwarevm/Ubuntu.vmdk": append([]byte("KDMV"), bytes.Repeat([]byte("vmdata--"), 256<<10)...),
			"Documents/notes.txt":                          []byte(strings.Repeat("Remember to back up the VMs.\n", 200)),
			"Photos/beach.png":                             pngBytes(400, 260, color.RGBA{30, 140, 220, 255}),
			"Music/tone.wav":                               wavBytes(),
		}
		del = map[string][]byte{
			"Virtual Machines/Windows.vmwarevm/Windows.vmx": []byte(".encoding = \"UTF-8\"\nmemsize = \"8192\"\n"),
			"Documents/report.pdf":                          pdfBytes("Quarterly report"),
			"Photos/forest.png":                             pngBytes(380, 240, color.RGBA{40, 150, 60, 255}),
		}
	} else {
		if out, err := run("newfs_msdos", "-F", "32", "-v", "LABDISK", strings.Replace(dev, "/dev/", "/dev/r", 1)); err != nil {
			return nil, fmt.Errorf("newfs_msdos: %s", out)
		}
		if out, err := run("diskutil", "mount", "-mountPoint", mnt, dev); err != nil {
			return nil, fmt.Errorf("mount: %s", out)
		}
		keep = map[string][]byte{"photo_1.png": pngBytes(360, 200, color.RGBA{40, 160, 70, 255}),
			"photo_3.png": pngBytes(440, 200, color.RGBA{240, 180, 20, 255})}
		del = map[string][]byte{"photo_0.png": pngBytes(320, 200, color.RGBA{220, 40, 40, 255}),
			"photo_2.png": pngBytes(400, 200, color.RGBA{40, 90, 220, 255}), "report.pdf": pdfBytes("Recovered by Disk Recovery Lab"),
			"tone.wav": wavBytes(), "archive.zip": zipBytes()}
	}
	if err := writeAll(mnt, keep); err != nil {
		return nil, err
	}
	if err := writeAll(mnt, del); err != nil {
		return nil, err
	}
	remount(vol, mnt) // flush data to the image before deleting
	res := &PracticeResult{Image: path, Damaged: kind == "damaged", Kept: []string{}, Deleted: []string{}}
	for rel := range del {
		os.Remove(filepath.Join(mnt, rel))
		res.Deleted = append(res.Deleted, rel)
	}
	if kind == "mac" {
		os.RemoveAll(filepath.Join(mnt, "Virtual Machines/Windows.vmwarevm"))
	}
	for rel := range keep {
		res.Kept = append(res.Kept, rel)
	}
	run("sync")
	run("diskutil", "unmount", mnt)
	run("hdiutil", "detach", dev)
	if kind == "damaged" { // wipe the boot sector and FSInfo; FAT32 keeps a backup at sector 6
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			f.WriteAt(make([]byte, 1024), 0)
			f.Close()
		}
	}
	ChownToUser(path)
	return res, nil
}
