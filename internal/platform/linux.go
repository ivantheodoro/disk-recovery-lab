//go:build linux

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const isDarwin = false

// Supported reports the optional tools available on Linux.
func Supported() Features {
	return Features{Mount: true, Fsck: true, OS: "linux",
		AdminHint: "Start the app again and approve the administrator prompt, or run it with sudo."}
}

type lsblkDev struct {
	Name, Path, Type, Mountpoint, Label, Fstype, Model, Tran, Pkname string
	Size                                                             int64
	Rm, Hotplug                                                      bool
	Children                                                         []lsblkDev
}

// Drives lists block devices using lsblk.
func Drives() ([]Drive, error) {
	out, err := exec.Command("lsblk", "-J", "-b", "-o", "NAME,PATH,SIZE,TYPE,MOUNTPOINT,LABEL,FSTYPE,MODEL,TRAN,RM,HOTPLUG").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var d struct{ Blockdevices []lsblkDev }
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, err
	}
	var drives []Drive
	for _, b := range d.Blockdevices {
		if b.Type != "disk" && b.Type != "loop" {
			continue
		}
		if b.Type == "loop" && b.Size == 0 {
			continue
		}
		drv := Drive{ID: b.Name, Device: b.Path, Size: b.Size, Content: b.Fstype, Media: strings.TrimSpace(b.Model),
			Bus: strings.ToUpper(b.Tran), Internal: !(b.Rm || b.Hotplug || b.Tran == "usb"), Removable: b.Rm || b.Hotplug,
			Partitions: []Partition{}}
		for _, c := range b.Children {
			drv.Partitions = append(drv.Partitions, Partition{ID: c.Name, Device: c.Path, Name: c.Label, Content: c.Fstype,
				Size: c.Size, Mount: c.Mountpoint})
		}
		drives = append(drives, drv)
	}
	return drives, nil
}

// MountReadOnly mounts a partition read-only via udisks.
func MountReadOnly(id string) MountResult {
	dev := "/dev/" + strings.TrimPrefix(id, "/dev/")
	out, err := run("udisksctl", "mount", "--no-user-interaction", "-b", dev, "-o", "ro")
	if err != nil {
		return MountResult{OK: false, Output: out, Explain: mountExplain}
	}
	mp := ""
	if i := strings.Index(out, " at "); i >= 0 {
		mp = strings.TrimSuffix(strings.TrimSpace(out[i+4:]), ".")
	}
	return MountResult{OK: true, Output: out, Mount: mp}
}

// Unmount unmounts a partition.
func Unmount(id string, force bool) (string, error) {
	args := []string{"unmount", "--no-user-interaction", "-b", "/dev/" + strings.TrimPrefix(id, "/dev/")}
	if force {
		args = append(args, "-f")
	}
	return run("udisksctl", args...)
}

// AttachImage is only available on macOS.
func AttachImage(string, bool) (map[string]any, error) { return nil, ErrUnsupported }

// Detach is only available on macOS.
func Detach(string) (string, error) { return "", ErrUnsupported }

var fsckCmd = map[string][]string{"ext2": {"e2fsck", "-n", "-f"}, "ext3": {"e2fsck", "-n", "-f"},
	"ext4": {"e2fsck", "-n", "-f"}, "vfat": {"fsck.vfat", "-n"}, "exfat": {"fsck.exfat", "-n"},
	"hfsplus": {"fsck.hfsplus", "-n"}, "ntfs": {"ntfsfix", "-n"}, "btrfs": {"btrfs", "check", "--readonly"}}

// FsckCommand returns the no-write checker for a partition.
func FsckCommand(id string) ([]string, error) {
	dev := "/dev/" + strings.TrimPrefix(id, "/dev/")
	out, err := exec.Command("lsblk", "-n", "-o", "FSTYPE,MOUNTPOINT", dev).Output()
	if err != nil {
		return nil, err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return nil, errors.New("unknown filesystem")
	}
	if len(f) > 1 {
		return nil, errors.New("the volume is mounted: unmount it first so the check sees a consistent disk")
	}
	cmd, ok := fsckCmd[f[0]]
	if !ok {
		return nil, fmt.Errorf("no read-only checker for filesystem %q", f[0])
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		return nil, fmt.Errorf("%s is not installed", cmd[0])
	}
	return append(append([]string{}, cmd...), dev), nil
}

// wholeOf maps a block device name (sdb1, nvme0n1p2) to its disk (sdb, nvme0n1).
func wholeOf(name string) string {
	link, err := filepath.EvalSymlinks("/sys/class/block/" + name)
	if err != nil {
		return name
	}
	if _, err := os.Stat(filepath.Join(link, "partition")); err == nil {
		return filepath.Base(filepath.Dir(link))
	}
	return filepath.Base(link)
}

// SameDisk reports whether dir is stored on the physical disk of src.
func SameDisk(src, dir string) bool {
	if !strings.HasPrefix(src, "/dev/") {
		return false
	}
	var st syscall.Stat_t
	if syscall.Stat(dir, &st) != nil {
		return false
	}
	dev := uint64(st.Dev) // glibc encoding of major/minor
	major := ((dev >> 8) & 0xfff) | ((dev >> 32) & 0xfffff000)
	minor := (dev & 0xff) | ((dev >> 12) & 0xffffff00)
	link, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", major, minor))
	if err != nil {
		return false
	}
	return wholeOf(filepath.Base(link)) == wholeOf(filepath.Base(src))
}

// Destinations lists mounted filesystems recovered files can be saved to.
func Destinations(src string) []Destination {
	var out []Destination
	seen := map[string]bool{}
	add := func(path, name string) {
		if seen[path] {
			return
		}
		seen[path] = true
		if free, err := FreeSpace(path); err == nil {
			out = append(out, Destination{Path: path, Name: name, Free: free, SameDisk: SameDisk(src, path)})
		}
	}
	add(HomeDir(), "Home folder")
	drives, _ := Drives()
	for _, d := range drives {
		for _, p := range d.Partitions {
			if p.Mount != "" && p.Mount != "/" && !strings.HasPrefix(p.Mount, "/boot") && !strings.HasPrefix(p.Mount, "[") {
				name := p.Name
				if name == "" {
					name = p.Mount
				}
				add(p.Mount, name)
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

// Reveal opens a folder in the file manager.
func Reveal(path string) error {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		path = filepath.Dir(path)
	}
	return asUser("xdg-open", path).Start()
}

// Elevate restarts the server through pkexec (graphical admin prompt).
func Elevate(a ServeArgs) error {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return errors.New("pkexec is not installed: run the app with sudo instead")
	}
	cmd := exec.Command("pkexec", append([]string{Exe()}, a.Flags()...)...)
	return cmd.Start()
}

var appBrowsers = []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"}

// OpenUI opens the interface in an app window when a Chromium browser exists.
func OpenUI(url string) error {
	for _, b := range appBrowsers {
		if p, err := exec.LookPath(b); err == nil {
			return asUser(p, "--app="+url).Start()
		}
	}
	return asUser("xdg-open", url).Start()
}
