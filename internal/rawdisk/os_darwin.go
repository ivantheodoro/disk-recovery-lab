package rawdisk

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	dkiocGetBlockSize  = 0x40046418 // _IOR('d', 24, uint32_t)
	dkiocGetBlockCount = 0x40086419 // _IOR('d', 25, uint64_t)
)

// IsDevicePath reports whether path names a disk device.
func IsDevicePath(path string) bool { return strings.HasPrefix(path, "/dev/") }

// DevicePath converts "disk5s2" or "/dev/disk5s2" to the faster raw node.
func DevicePath(id string) string {
	id = strings.TrimPrefix(strings.TrimPrefix(id, "/dev/"), "r")
	return "/dev/r" + id
}

func openReadOnly(path string) (*os.File, error) { return os.Open(path) }

func deviceSize(f *os.File) (int64, error) {
	var bs uint32
	var cnt uint64
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), dkiocGetBlockSize, uintptr(unsafe.Pointer(&bs))); e != 0 {
		return 0, e
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), dkiocGetBlockCount, uintptr(unsafe.Pointer(&cnt))); e != 0 {
		return 0, e
	}
	return int64(bs) * int64(cnt), nil
}
