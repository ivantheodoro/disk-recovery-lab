//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ivantheodoro/disk-recovery-lab/internal/plist"
)

// Supported reports the optional tools available on macOS.
func Supported() Features {
	return Features{Mount: true, Attach: true, Fsck: true, Practice: true, OS: "macos",
		AdminHint: "Open the app again and enter your Mac password when asked."}
}

func plistOf(args ...string) (map[string]any, error) {
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	v, err := plist.Decode(out)
	return plist.Dict(v), err
}

func diskInfo(id string) map[string]any {
	m, _ := plistOf("diskutil", "info", "-plist", id)
	return m
}

// Drives lists disks using diskutil.
func Drives() ([]Drive, error) {
	all, err := plistOf("diskutil", "list", "-plist")
	if err != nil {
		return nil, err
	}
	var out []Drive
	for _, dv := range plist.Arr(all["AllDisksAndPartitions"]) {
		d := plist.Dict(dv)
		id := plist.Str(d["DeviceIdentifier"])
		info := diskInfo(id)
		drv := Drive{ID: id, Device: "/dev/r" + id, Size: plist.Int(d["Size"]), Content: plist.Str(d["Content"]),
			Media: plist.Str(info["MediaName"]), Bus: plist.Str(info["BusProtocol"]), Internal: plist.Bool(info["Internal"]),
			Removable: plist.Bool(info["RemovableMediaOrExternalDevice"]), Synthesized: d["APFSPhysicalStores"] != nil,
			Partitions: []Partition{}}
		for _, pv := range append(plist.Arr(d["Partitions"]), plist.Arr(d["APFSVolumes"])...) {
			p := plist.Dict(pv)
			pid := plist.Str(p["DeviceIdentifier"])
			drv.Partitions = append(drv.Partitions, Partition{ID: pid, Device: "/dev/r" + pid, Name: plist.Str(p["VolumeName"]),
				Content: plist.Str(p["Content"]), Size: plist.Int(p["Size"]), Mount: plist.Str(p["MountPoint"])})
		}
		out = append(out, drv)
	}
	return out, nil
}

// MountReadOnly asks macOS to mount a volume without writing to it.
func MountReadOnly(id string) MountResult {
	id = strings.TrimPrefix(strings.Replace(id, "/dev/r", "/dev/", 1), "/dev/")
	out, err := run("diskutil", "mount", "readonly", id)
	if err != nil {
		return MountResult{OK: false, Output: out, Explain: mountExplain}
	}
	return MountResult{OK: true, Output: out, Mount: plist.Str(diskInfo(id)["MountPoint"])}
}

// Unmount unmounts a volume.
func Unmount(id string, force bool) (string, error) {
	args := []string{"unmount"}
	if force {
		args = append(args, "force")
	}
	return run("diskutil", append(args, id)...)
}

// AttachImage attaches a disk image read-only and hidden from Finder.
func AttachImage(path string, mount bool) (map[string]any, error) {
	args := []string{"attach", "-readonly", "-nobrowse", "-plist"}
	if !regexp.MustCompile(`(?i)\.(dmg|sparseimage|sparsebundle)$`).MatchString(path) {
		args = append(args, "-imagekey", "diskimage-class=CRawDiskImage")
	}
	note := ""
	try := func(extra ...string) (map[string]any, error) {
		return plistOf(append(append([]string{"hdiutil"}, append(args, extra...)...), path)...)
	}
	var m map[string]any
	var err error
	if mount {
		m, err = try()
	}
	if !mount || err != nil {
		if m, err = try("-nomount"); err != nil {
			return nil, err
		}
		if mount {
			note = "macOS found no mountable filesystem; attached as a raw device only."
		}
	}
	var devs []map[string]string
	for _, e := range plist.Arr(m["system-entities"]) {
		ed := plist.Dict(e)
		devs = append(devs, map[string]string{"dev": plist.Str(ed["dev-entry"]), "content": plist.Str(ed["content-hint"]),
			"mount": plist.Str(ed["mount-point"])})
	}
	return map[string]any{"note": note, "devices": devs}, nil
}

// Detach ejects an attached image.
func Detach(dev string) (string, error) { return run("hdiutil", "detach", dev) }

var fsckCmd = map[string][]string{"hfs": {"fsck_hfs", "-n", "-f"}, "msdos": {"fsck_msdos", "-n"},
	"exfat": {"fsck_exfat", "-n"}, "apfs": {"fsck_apfs", "-n"}}

// FsckCommand returns the no-write checker for a partition.
func FsckCommand(id string) ([]string, error) {
	id = strings.TrimPrefix(strings.Replace(id, "/dev/r", "/dev/", 1), "/dev/")
	info := diskInfo(id)
	kind := plist.Str(info["FilesystemType"])
	cmd, ok := fsckCmd[kind]
	if !ok {
		return nil, fmt.Errorf("no read-only checker for filesystem %q", kind)
	}
	if plist.Str(info["MountPoint"]) != "" {
		return nil, fmt.Errorf("the volume is mounted: unmount it first so the check sees a consistent disk")
	}
	return append(append([]string{}, cmd...), "/dev/r"+plist.Str(info["DeviceIdentifier"])), nil
}

func wholeDisk(path string) string {
	m := regexp.MustCompile(`/dev/r?(disk\d+)`).FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return m[1]
}

// SameDisk reports whether dir is stored on the physical disk of src.
func SameDisk(src, dir string) bool {
	sd := wholeDisk(src)
	if sd == "" { // an image file: writing next to it is fine, it is already a copy
		return false
	}
	info := diskInfo(dir)
	parents := map[string]bool{plist.Str(info["ParentWholeDisk"]): true}
	stores := plist.Arr(info["APFSPhysicalStores"])
	if ref := plist.Str(info["APFSContainerReference"]); ref != "" {
		stores = append(stores, plist.Arr(diskInfo(ref)["APFSPhysicalStores"])...)
	}
	for _, s := range stores {
		parents[wholeDisk("/dev/"+plist.Str(plist.Dict(s)["APFSPhysicalStore"]))] = true
	}
	return parents[sd]
}

var systemVolumes = map[string]bool{"Recovery": true, "Preboot": true, "VM": true, "Update": true, "xART": true,
	"iSCPreboot": true, "Hardware": true}

// Destinations lists mounted volumes recovered files can be saved to.
func Destinations(src string) []Destination {
	drives, _ := Drives()
	var out []Destination
	seen := map[string]bool{}
	for _, d := range drives {
		for _, p := range d.Partitions {
			if p.Mount == "" || systemVolumes[p.Name] {
				continue
			}
			path, name := p.Mount, p.Name
			if p.Mount == "/" || p.Mount == "/System/Volumes/Data" {
				path, name = filepath.Join(HomeDir(), "Desktop"), "This Mac (Desktop)"
			} else if strings.HasPrefix(p.Mount, "/System/") {
				continue
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			if free, err := FreeSpace(path); err == nil {
				out = append(out, Destination{Path: path, Name: name, Free: free, SameDisk: SameDisk(src, path)})
			}
		}
	}
	return out
}

// FreeSpace returns the free bytes available at path.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Reveal shows a file or folder in Finder.
func Reveal(path string) error {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return asUser("open", "-R", path).Run()
	}
	return asUser("open", path).Run()
}
