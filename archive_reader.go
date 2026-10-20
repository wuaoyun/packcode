package main

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

const archiveBlockSize = 1 << 20

type archiveBlock struct {
	sync.Mutex
	offset int64
	data   []byte
	valid  bool
	err    error
}
type archiveReader struct {
	file   *os.File
	blocks [16]archiveBlock
}

var copyMappedMemory = syscall.NewLazyDLL("kernel32.dll").NewProc("RtlMoveMemory")

type mappedArchive struct {
	address uintptr
	size    int64
}

// Keep the mapping address in native address form; only the destination is a Go
// buffer. No Go slice points at unmanaged memory, and all reads are bounds checked.
func (m mappedArchive) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, os.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= m.size {
		return 0, io.EOF
	}
	n := len(p)
	var err error
	if int64(n) > m.size-off {
		n = int(m.size - off)
		err = io.EOF
	}
	copyMappedMemory.Call(uintptr(unsafe.Pointer(&p[0])), m.address+uintptr(off), uintptr(n))
	runtime.KeepAlive(p)
	return n, err
}

// ZIP normally performs separate short ReadAt calls for every local header and
// payload. Cache 1 MiB blocks to turn adjacent tiny reads into sequential reads.
// Direct-mapped slots bound memory to 16 MiB regardless of ZIP size, and separate
// slot locks permit parallel access without one global I/O lock.
func (r *archiveReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, os.ErrInvalid
	}
	total := 0
	for len(p) > 0 {
		base := off / int64(archiveBlockSize) * int64(archiveBlockSize)
		b := &r.blocks[(base/int64(archiveBlockSize))%int64(len(r.blocks))]
		b.Lock()
		if !b.valid || b.offset != base {
			if cap(b.data) < archiveBlockSize {
				b.data = make([]byte, archiveBlockSize)
			}
			b.data = b.data[:archiveBlockSize]
			n, e := r.file.ReadAt(b.data, base)
			b.data = b.data[:n]
			b.err = e
			b.offset = base
			b.valid = true
		}
		start := int(off - base)
		if start >= len(b.data) {
			e := b.err
			b.Unlock()
			if e == nil {
				e = io.EOF
			}
			return total, e
		}
		n := copy(p, b.data[start:])
		b.Unlock()
		p = p[n:]
		off += int64(n)
		total += n
	}
	return total, nil
}

func openArchive(ctx context.Context, path string) (*zip.Reader, func(), error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, nil, e
	}
	s, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, nil, e
	}
	if e = checkpoint(ctx); e != nil {
		f.Close()
		return nil, nil, e
	}
	// Read-only mapping lets concurrent ZIP members read the OS page cache without
	// per-member system calls or cache-slot locks. Pages are loaded on demand; a
	// large archive does not require allocating a same-sized Go byte buffer.
	var reader io.ReaderAt = &archiveReader{file: f}
	closeArchive := func() { f.Close() }
	if s.Size() > 0 && uint64(s.Size()) <= uint64(^uint(0)>>1) {
		mapping, mapErr := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, 0, 0, nil)
		if mapErr == nil {
			addr, viewErr := syscall.MapViewOfFile(mapping, syscall.FILE_MAP_READ, 0, 0, 0)
			if viewErr == nil {
				reader = mappedArchive{addr, s.Size()}
				closeArchive = func() { syscall.UnmapViewOfFile(addr); syscall.CloseHandle(mapping); f.Close() }
			} else {
				syscall.CloseHandle(mapping)
			}
		}
	}
	z, e := zip.NewReader(reader, s.Size())
	if e != nil {
		closeArchive()
		return nil, nil, e
	}
	return z, closeArchive, nil
}
