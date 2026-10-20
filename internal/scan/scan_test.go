package scan

import (
	"bytes"
	"compress/gzip"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ivantheodoro/disk-recovery-lab/internal/detect"
	"github.com/ivantheodoro/disk-recovery-lab/internal/hfs"
	"github.com/ivantheodoro/disk-recovery-lab/internal/rawdisk"
)

type testJob struct{ t *testing.T }

func (testJob) Check() error        { return nil }
func (j testJob) Log(m string)      { j.t.Log(m) }
func (testJob) SetProgress(float64) {}
func (testJob) SetFound(int)        {}

// fixture unpacks testdata/<name>.gz into a temp file.
func fixture(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name+".gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), name)
	w, _ := os.Create(out)
	if _, err := io.Copy(w, zr); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return out
}

func files(s *Scan) []string {
	var out []string
	for _, it := range s.Items {
		if it.Kind == "file" && it.Origin == "fs" && !it.Deleted {
			out = append(out, strings.Join(s.PathOf(it.ID)[1:], "/"))
		}
	}
	sort.Strings(out)
	return out
}

var wantHFS = []string{"Documents/Work/report.txt", "Photos/green.png", "Photos/red.jpg", "Photos/red.png", "VMs/Test.vmwarevm/Test.vmx"}

func TestQuickScanHFS(t *testing.T) {
	img := fixture(t, "hfs.img")
	s := New(img, "fixture")
	if err := Run(s, testJob{t}, false, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := files(s); strings.Join(got, ",") != strings.Join(wantHFS, ",") {
		t.Fatalf("files = %v, want %v", got, wantHFS)
	}
	if name := s.Items[1].Name; name != "FIXTURE (HFSX)" {
		t.Errorf("volume label = %q", name)
	}
}

// A zeroed catalog header node is what makes macOS refuse to mount; the
// scanner must still rebuild the file list.
func TestDamagedCatalogHeader(t *testing.T) {
	img := fixture(t, "hfs.img")
	r, _ := rawdisk.Open(img)
	_, parts := detect.PartitionTable(r)
	var off int64
	for _, p := range parts {
		if strings.HasPrefix(p.Type, "Apple HFS") {
			off = p.Offset
		}
	}
	vh := r.MustRead(off+1024, 512)
	bs := int64(vh[40])<<24 | int64(vh[41])<<16 | int64(vh[42])<<8 | int64(vh[43])
	catStart := int64(vh[272+16])<<24 | int64(vh[272+17])<<16 | int64(vh[272+18])<<8 | int64(vh[272+19])
	r.Close()
	f, _ := os.OpenFile(img, os.O_WRONLY, 0)
	f.WriteAt(make([]byte, 4096), off+catStart*bs)
	f.Close()

	s := New(img, "damaged")
	if err := Run(s, testJob{t}, false, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := files(s); strings.Join(got, ",") != strings.Join(wantHFS, ",") {
		t.Fatalf("files = %v, want %v", got, wantHFS)
	}
	if !strings.Contains(strings.Join(s.Notes, " "), "header is damaged") {
		t.Error("expected a note about the damaged header")
	}
}

func TestDeepScanAndRecover(t *testing.T) {
	img := fixture(t, "hfs.img")
	dir := t.TempDir()
	s := New(img, "fixture")
	if err := Run(s, testJob{t}, true, dir); err != nil {
		t.Fatal(err)
	}
	var carved, pngID int
	for _, it := range s.Items {
		if it.Origin == "carved" {
			carved++
		}
		if it.Name == "red.png" {
			pngID = it.ID
		}
	}
	if carved == 0 {
		t.Error("deep scan found nothing in free space (deleted-blue.png and deleted.pdf were deleted there)")
	}
	dest := t.TempDir()
	res, err := Recover(s, []int{pngID}, dest, testJob{t})
	if err != nil || res.Files != 1 {
		t.Fatalf("recover: %v %+v", err, res)
	}
	b, err := os.ReadFile(filepath.Join(res.OutDir, "FIXTURE (HFSX)", "Photos", "red.png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("recovered PNG does not decode: %v", err)
	}
	// Saved scans load back identically.
	l, err := Load(dir, s.ID)
	if err != nil || len(l.Items) != len(s.Items) || l.Meta().Files != s.Meta().Files {
		t.Fatalf("load: %v", err)
	}
}

func TestLostPartitionAndRepair(t *testing.T) {
	img := fixture(t, "fat32-wiped.img")
	p, err := detect.Diagnose(img)
	if err != nil || !p.Sector0Zero || p.WholeDiskFS != nil {
		t.Fatalf("diagnose: %v %+v", err, p)
	}
	found, err := detect.SearchLostPartitions(img, testJob{t})
	if err != nil || len(found) == 0 || found[0].BackupOf == nil || *found[0].BackupOf != 0 {
		t.Fatalf("lost partitions: %v %+v", err, found)
	}
	dst := filepath.Join(t.TempDir(), "repaired.img")
	if _, err := detect.RepairedCopy(img, dst, 0, found[0].Offset, "FAT32", testJob{t}); err != nil {
		t.Fatal(err)
	}
	r, _ := rawdisk.Open(dst)
	defer r.Close()
	if fs := detect.DetectFS(r, 0); fs == nil || fs.Name != "FAT32" {
		t.Fatalf("repaired copy not recognised: %+v", fs)
	}
	// The source must be untouched.
	src, _ := rawdisk.Open(img)
	defer src.Close()
	if detect.DetectFS(src, 0) != nil {
		t.Fatal("source was modified")
	}
}

func TestSafeName(t *testing.T) {
	want := map[string]string{"a/b": "a:b", "..": "_..", "": "_", "ok.txt": "ok.txt"}
	if runtime.GOOS == "windows" { // ':' is invalid and trailing dots are dropped by Windows
		want = map[string]string{"a/b": "a_b", "..": "_", "": "_", "ok.txt": "ok.txt", "CON.txt": "_CON.txt", "a?b": "a_b"}
	}
	for in, w := range want {
		if got := SafeName(in); got != w {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, w)
		}
	}
}

var _ = hfs.Epoch
