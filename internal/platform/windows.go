//go:build windows

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
)

// Supported reports the optional tools available on Windows.
func Supported() Features {
	return Features{Fsck: true, OS: "windows", AdminHint: "Open the app again and choose Yes in the Windows prompt."}
}

// IsAdmin reports whether the process is elevated.
func IsAdmin() bool {
	r, _, _ := shell32.NewProc("IsUserAnAdmin").Call()
	return r != 0
}

func asUser(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func chownTree(string, int, int) {}

// CurrentUser returns uid, gid and home of the person launching the app.
func CurrentUser() (int, int, string) {
	h, _ := os.UserHomeDir()
	return 0, 0, h
}

func powershell(script string) ([]byte, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Output()
}

// asList decodes JSON that PowerShell prints as an object or an array.
func asList(b []byte) []map[string]any {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil
	}
	var list []map[string]any
	if b[0] == '{' {
		var one map[string]any
		if json.Unmarshal(b, &one) == nil {
			return []map[string]any{one}
		}
	}
	json.Unmarshal(b, &list)
	return list
}

func num(v any) int64 { f, _ := v.(float64); return int64(f) }
func str(v any) string {
	s, _ := v.(string)
	return strings.Trim(s, "\x00 ")
}

// Drives lists physical disks and their partitions through PowerShell.
func Drives() ([]Drive, error) {
	// Enums are turned into strings explicitly: Windows PowerShell 5.1 would print numbers.
	disks, err := powershell(`Get-Disk | Select-Object Number,FriendlyName,Size,IsBoot,IsSystem,@{n='BusType';e={"$($_.BusType)"}},@{n='PartitionStyle';e={"$($_.PartitionStyle)"}} | ConvertTo-Json -Compress`)
	if err != nil {
		return nil, fmt.Errorf("Get-Disk: %w", err)
	}
	parts, _ := powershell(`Get-Partition | Select-Object DiskNumber,PartitionNumber,Offset,Size,@{n='DriveLetter';e={"$($_.DriveLetter)".Trim([char]0)}},@{n='Type';e={"$($_.Type)"}} | ConvertTo-Json -Compress`)
	vols, _ := powershell(`Get-Volume | Where-Object DriveLetter | Select-Object @{n='DriveLetter';e={"$($_.DriveLetter)"}},FileSystemLabel,FileSystem | ConvertTo-Json -Compress`)
	volBy := map[string]map[string]any{}
	for _, v := range asList(vols) {
		volBy[str(v["DriveLetter"])] = v
	}
	var out []Drive
	for _, d := range asList(disks) {
		n := num(d["Number"])
		bus := fmt.Sprint(d["BusType"])
		drv := Drive{ID: strconv.FormatInt(n, 10), Device: fmt.Sprintf(`\\.\PhysicalDrive%d`, n), Size: num(d["Size"]),
			Content: fmt.Sprint(d["PartitionStyle"]), Media: fmt.Sprint(d["FriendlyName"]), Bus: bus,
			Internal: d["IsBoot"] == true || d["IsSystem"] == true, Removable: bus == "USB" || bus == "SD" || bus == "MMC",
			Partitions: []Partition{}}
		for _, p := range asList(parts) {
			if num(p["DiskNumber"]) != n {
				continue
			}
			letter := str(p["DriveLetter"])
			part := Partition{ID: fmt.Sprintf("%d:%d", n, num(p["PartitionNumber"])), Device: drv.Device,
				Offset: num(p["Offset"]), Size: num(p["Size"]), Content: fmt.Sprint(p["Type"])}
			if v := volBy[letter]; v != nil && letter != "" {
				part.Mount = letter + `:\`
				part.Name = fmt.Sprint(v["FileSystemLabel"])
				part.Content = fmt.Sprint(v["FileSystem"])
			}
			drv.Partitions = append(drv.Partitions, part)
		}
		out = append(out, drv)
	}
	return out, nil
}

// MountReadOnly is not available: Windows cannot mount foreign filesystems.
func MountReadOnly(string) MountResult {
	return MountResult{OK: false, Output: "Windows cannot mount this filesystem.", Explain: "Windows only mounts NTFS, FAT and " +
		"exFAT. For other filesystems (for example a Mac drive), scan the partition: the scan reads the files directly."}
}

// Unmount is not available on Windows.
func Unmount(string, bool) (string, error) { return "", ErrUnsupported }

// AttachImage is only available on macOS.
func AttachImage(string, bool) (map[string]any, error) { return nil, ErrUnsupported }

// Detach is only available on macOS.
func Detach(string) (string, error) { return "", ErrUnsupported }

// FsckCommand returns chkdsk without /f, which only reports problems.
func FsckCommand(id string) ([]string, error) {
	drives, _ := Drives()
	for _, d := range drives {
		for _, p := range d.Partitions {
			if p.ID == id && p.Mount != "" {
				return []string{"chkdsk", strings.TrimSuffix(p.Mount, `\`)}, nil
			}
		}
	}
	return nil, errors.New("the check needs a partition with a drive letter")
}

var physRe = regexp.MustCompile(`(?i)PhysicalDrive(\d+)`)

// SameDisk reports whether dir is stored on the physical disk of src.
func SameDisk(src, dir string) bool {
	m := physRe.FindStringSubmatch(src)
	abs, _ := filepath.Abs(dir)
	if m == nil || len(abs) < 2 || abs[1] != ':' {
		return false
	}
	out, err := powershell(fmt.Sprintf(`(Get-Partition -DriveLetter %c).DiskNumber`, abs[0]))
	return err == nil && strings.TrimSpace(string(out)) == m[1]
}

// Destinations lists drive letters recovered files can be saved to.
func Destinations(src string) []Destination {
	var out []Destination
	if h := HomeDir(); h != "" {
		desk := filepath.Join(h, "Desktop")
		if free, err := FreeSpace(desk); err == nil {
			out = append(out, Destination{Path: desk, Name: "This PC (Desktop)", Free: free, SameDisk: SameDisk(src, desk)})
		}
	}
	vols, _ := powershell(`Get-Volume | Where-Object DriveLetter | Select-Object @{n='DriveLetter';e={"$($_.DriveLetter)"}},FileSystemLabel | ConvertTo-Json -Compress`)
	for _, v := range asList(vols) {
		root := str(v["DriveLetter"]) + `:\`
		if free, err := FreeSpace(root); err == nil {
			name := fmt.Sprint(v["FileSystemLabel"])
			if name == "" {
				name = "Local Disk"
			}
			out = append(out, Destination{Path: root, Name: name + " (" + root + ")", Free: free, SameDisk: SameDisk(src, root)})
		}
	}
	return out
}

// FreeSpace returns the free bytes available at path.
func FreeSpace(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free int64
	r, _, e := kernel32.NewProc("GetDiskFreeSpaceExW").Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), 0, 0)
	if r == 0 {
		return 0, e
	}
	return free, nil
}

// Reveal shows a file or folder in Explorer.
func Reveal(path string) error {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return exec.Command("explorer", "/select,", path).Start()
	}
	return exec.Command("explorer", path).Start()
}

// Elevate restarts the program with administrator rights (UAC prompt).
func Elevate(a ServeArgs) error {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(Exe())
	args := ""
	for _, f := range a.Flags() {
		args += syscall.EscapeArg(f) + " "
	}
	params, _ := syscall.UTF16PtrFromString(args)
	r, _, _ := shell32.NewProc("ShellExecuteW").Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(exe)),
		uintptr(unsafe.Pointer(params)), 0, 0) // SW_HIDE
	if r <= 32 {
		if r == 5 { // ERROR_ACCESS_DENIED: the user said No
			return errors.New("cancelled")
		}
		return fmt.Errorf("could not start with administrator rights (code %d)", r)
	}
	return nil
}

// OpenUI opens the interface in an Edge/Chrome app window, or the default browser.
func OpenUI(url string) error {
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		for _, rel := range []string{`Microsoft\Edge\Application\msedge.exe`, `Google\Chrome\Application\chrome.exe`} {
			if p := filepath.Join(base, rel); base != "" {
				if _, err := os.Stat(p); err == nil {
					return exec.Command(p, "--app="+url).Start()
				}
			}
		}
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
