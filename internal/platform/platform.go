// Package platform hides the per-OS parts: listing drives, mounting,
// checking filesystems, administrator rights and opening the UI window.
package platform

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// ErrUnsupported is returned for features that do not exist on this OS.
var ErrUnsupported = errors.New("not available on this operating system")

// Partition is a partition or volume as the OS sees it.
type Partition struct {
	ID      string `json:"id"`     // e.g. disk5s2, sdb1, 1:2
	Device  string `json:"device"` // path to open for scanning
	Offset  int64  `json:"offset"` // byte offset inside the whole-disk device (Windows)
	Name    string `json:"name"`
	Content string `json:"content"` // filesystem or partition type
	Size    int64  `json:"size"`
	Mount   string `json:"mount"`
}

// Drive is a physical (or attached image) disk.
type Drive struct {
	ID          string      `json:"id"`
	Device      string      `json:"device"`
	Size        int64       `json:"size"`
	Content     string      `json:"content"`
	Media       string      `json:"media"`
	Bus         string      `json:"bus"`
	Internal    bool        `json:"internal"`
	Removable   bool        `json:"removable"`
	Synthesized bool        `json:"synthesized"`
	Partitions  []Partition `json:"partitions"`
}

// MountResult explains the outcome of a read-only mount attempt.
type MountResult struct {
	OK      bool   `json:"ok"`
	Output  string `json:"output"`
	Mount   string `json:"mount"`
	Explain string `json:"explain,omitempty"`
}

// Destination is a place recovered files can be written to.
type Destination struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Free     int64  `json:"free"`
	SameDisk bool   `json:"same_disk"`
}

// Features tells the UI which optional tools exist on this OS.
type Features struct {
	Mount     bool   `json:"mount"`
	Attach    bool   `json:"attach"`
	Fsck      bool   `json:"fsck"`
	Practice  bool   `json:"practice"`
	OS        string `json:"os"`
	AdminHint string `json:"admin_hint"`
}

const mountExplain = "The operating system refused to mount this volume. For a filesystem it supports, this " +
	"almost always means the volume's internal structures are damaged. Your files are usually still there: " +
	"scan the partition from the home screen. The scan reads the file list itself, without needing the OS to mount it."

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// HomeDir returns the home folder of the person running the app, even when
// it was elevated with sudo/pkexec.
func HomeDir() string {
	if h := os.Getenv("DRL_USER_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}
