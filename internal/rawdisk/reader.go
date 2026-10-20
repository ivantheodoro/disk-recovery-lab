// Package rawdisk gives read-only, sector-aligned access to drives and image
// files on macOS, Linux and Windows.
//
// Raw devices (/dev/rdiskN, /dev/sdX, \\.\PhysicalDriveN) only accept reads
// whose offset and length are multiples of the sector size, so every read is
// widened to Align bytes and served from a small cache. Nothing in this
// package can write to a source: devices and files are opened read-only.
package rawdisk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	// Sector is the smallest unit used when stepping through a disk.
	Sector = 512
	// Align is the read alignment; 4096 is safe for 512e and 4Kn drives.
	Align    = 4096
	cacheMin = 1 << 20
)

// Reader reads a device or image file. It is safe for concurrent use.
type Reader struct {
	Path     string
	Size     int64
	IsDevice bool

	f     *os.File
	mu    sync.Mutex
	cOff  int64
	cData []byte
}

// Open opens path read-only and determines its size.
func Open(path string) (*Reader, error) {
	f, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	r := &Reader{Path: path, f: f, cOff: -1, IsDevice: IsDevicePath(path)}
	if r.IsDevice {
		r.Size, err = deviceSize(f)
	} else {
		var st os.FileInfo
		if st, err = f.Stat(); err == nil {
			r.Size = st.Size()
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("size of %s: %w", path, err)
	}
	return r, nil
}

// Close releases the underlying handle.
func (r *Reader) Close() error { return r.f.Close() }

// ReadRaw reads straight from the source at an aligned offset with an
// aligned length, bypassing the cache. Short reads at the end are allowed.
func (r *Reader) ReadRaw(off int64, length int) ([]byte, error) {
	if off >= r.Size || length <= 0 {
		return nil, nil
	}
	if rem := r.Size - off; int64(length) > rem {
		length = int(rem)
	}
	buf := make([]byte, length)
	n, err := r.f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return buf[:n], err
	}
	return buf[:n], nil
}

// Read returns up to length bytes at any offset (no alignment needed).
func (r *Reader) Read(off int64, length int) ([]byte, error) {
	if off < 0 || off >= r.Size || length <= 0 {
		return nil, nil
	}
	if rem := r.Size - off; int64(length) > rem {
		length = int(rem)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cOff >= 0 && off >= r.cOff && off+int64(length) <= r.cOff+int64(len(r.cData)) {
		s := off - r.cOff
		return append([]byte(nil), r.cData[s:s+int64(length)]...), nil
	}
	start := off - off%Align
	end := off + int64(length)
	end += (Align - end%Align) % Align
	want := end - start
	if want < cacheMin {
		want = cacheMin
	}
	if start+want > r.Size {
		want = r.Size - start
	}
	want -= want % Sector
	if want <= 0 {
		want = r.Size - start
	}
	data, err := r.ReadRaw(start, int(want))
	if err != nil {
		return nil, err
	}
	r.cOff, r.cData = start, data
	s := off - start
	if s >= int64(len(data)) {
		return nil, nil
	}
	e := s + int64(length)
	if e > int64(len(data)) {
		e = int64(len(data))
	}
	return append([]byte(nil), data[s:e]...), nil
}

// MustRead is Read that turns errors into an empty result; parsers use it
// when a missing byte simply means "no match".
func (r *Reader) MustRead(off int64, length int) []byte {
	b, _ := r.Read(off, length)
	return b
}
