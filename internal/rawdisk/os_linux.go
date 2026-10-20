package rawdisk

import (
	"io"
	"os"
	"strings"
)

// IsDevicePath reports whether path names a disk device.
func IsDevicePath(path string) bool { return strings.HasPrefix(path, "/dev/") }

// DevicePath converts "sdb1" or "/dev/sdb1" to a device node path.
func DevicePath(id string) string { return "/dev/" + strings.TrimPrefix(id, "/dev/") }

func openReadOnly(path string) (*os.File, error) { return os.Open(path) }

// Block devices report their size through lseek(SEEK_END).
func deviceSize(f *os.File) (int64, error) { return f.Seek(0, io.SeekEnd) }
