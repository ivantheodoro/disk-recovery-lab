package rawdisk

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const ioctlDiskGetLengthInfo = 0x0007405C

// IsDevicePath reports whether path names a disk device.
func IsDevicePath(path string) bool { return strings.HasPrefix(path, `\\.\`) }

// DevicePath converts "PhysicalDrive1" or "1" to \\.\PhysicalDrive1.
func DevicePath(id string) string {
	id = strings.TrimPrefix(id, `\\.\`)
	if id != "" && id[0] >= '0' && id[0] <= '9' {
		id = "PhysicalDrive" + id
	}
	return `\\.\` + id
}

// openReadOnly opens with GENERIC_READ and full sharing so a disk that is in
// use by Windows can still be read.
func openReadOnly(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func deviceSize(f *os.File) (int64, error) {
	var length int64
	var ret uint32
	err := syscall.DeviceIoControl(syscall.Handle(f.Fd()), ioctlDiskGetLengthInfo, nil, 0,
		(*byte)(unsafe.Pointer(&length)), 8, &ret, nil)
	return length, err
}
