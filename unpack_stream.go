package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	sevenreader "github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

type extractionSession struct {
	ctx      context.Context
	stage    string
	seen     map[string]bool
	dirs     map[string]bool
	buf      []byte
	progress Progress
	start    time.Time
	report   func(Progress)
}

// Every format uses the same path/type checks and exclusive creation policy.
// No archive member can create a link or write outside this task's staging tree.
func (s *extractionSession) add(name string, mode fs.FileMode, size int64, reader io.Reader) error {
	if e := checkpoint(s.ctx); e != nil {
		return e
	}
	s.progress.Scanned++
	if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
		return fmt.Errorf("不支持的压缩包成员类型：%s", name)
	}
	name = strings.ReplaceAll(name, "\\", "/")
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if mode.IsDir() && (name == "" || name == ".") {
		return nil
	}
	path, e := safeMember(name)
	if e != nil {
		return e
	}
	key := strings.ToLower(path)
	if _, ok := s.seen[key]; ok {
		return fmt.Errorf("压缩包含重复路径：%s", name)
	}
	s.seen[key] = mode.IsDir()
	if mode.IsDir() {
		if e = s.mkdir(path); e != nil {
			return e
		}
	} else {
		if s.dirs[key] {
			return fmt.Errorf("文件与目录路径冲突：%s", name)
		}
		if e = s.mkdir(filepath.Dir(path)); e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(s.stage, path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		n, e := io.CopyBuffer(writeOnly{f}, contextReader{s.ctx, reader}, s.buf)
		closeErr := f.Close()
		if e != nil {
			return fmt.Errorf("%s：%w", name, e)
		}
		if closeErr != nil {
			return closeErr
		}
		if size >= 0 && n != size {
			return fmt.Errorf("文件长度不一致：%s，预期 %d，实际 %d", name, size, n)
		}
		s.progress.Files++
		s.progress.Bytes += n
	}
	s.progress.Elapsed = time.Since(s.start)
	if s.report != nil {
		s.report(s.progress)
	}
	return nil
}

func (s *extractionSession) mkdir(path string) error {
	if path == "." || path == "" {
		return nil
	}
	key := strings.ToLower(path)
	if s.dirs[key] {
		return nil
	}
	if isDir, seen := s.seen[key]; seen && !isDir {
		return fmt.Errorf("文件与目录路径冲突：%s", path)
	}
	if e := s.mkdir(filepath.Dir(path)); e != nil {
		return e
	}
	if e := os.Mkdir(filepath.Join(s.stage, path), 0755); e != nil {
		return e
	}
	s.dirs[key] = true
	return nil
}

func unpackStream(ctx context.Context, source, destination, format string, report func(Progress)) (err error) {
	out, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, e := os.Lstat(out); e == nil {
		return fmt.Errorf("目标文件夹已存在，请选择新的文件夹")
	} else if !os.IsNotExist(e) {
		return e
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".packcode-extract-*")
	if err != nil {
		return err
	}
	defer func() {
		rel, e := filepath.Rel(filepath.Dir(out), stage)
		if e != nil || filepath.Dir(rel) != "." || !strings.HasPrefix(rel, ".packcode-extract-") {
			err = errors.Join(err, fmt.Errorf("%w：清理路径校验失败", errCleanupFailed))
			return
		}
		if e = os.RemoveAll(stage); e != nil {
			err = errors.Join(err, fmt.Errorf("%w：%s：%v", errCleanupFailed, stage, e))
		}
	}()
	s := &extractionSession{ctx: ctx, stage: stage, seen: map[string]bool{}, dirs: map[string]bool{}, buf: make([]byte, 256<<10), start: time.Now(), report: report}
	switch format {
	case "7z":
		z, e := sevenreader.OpenReader(source)
		if e != nil {
			// A standards-compliant empty 7z has only its 32-byte signature.
			// The decoder expects a next-header ID even when its length is zero.
			if validEmpty7z(source) {
				break
			}
			return e
		}
		defer z.Close()
		for _, f := range z.File {
			if !f.FileInfo().IsDir() {
				s.progress.Total++
			}
		}
		// Sequential archive order lets solid archives reuse decoded folder state
		// rather than independently decoding their shared streams in every worker.
		for _, f := range z.File {
			if e = checkpoint(ctx); e != nil {
				return e
			}
			fi := f.FileInfo()
			if f.Attributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return fmt.Errorf("不支持 7z 目录联接或链接：%s", f.Name)
			}
			if f.UncompressedSize > math.MaxInt64 {
				return fmt.Errorf("7z 成员过大：%s", f.Name)
			}
			if fi.IsDir() {
				if e = s.add(f.Name, fi.Mode(), 0, nil); e != nil {
					return e
				}
				continue
			}
			r, e := f.Open()
			if e != nil {
				return e
			}
			crc := crc32.NewIEEE()
			e = s.add(f.Name, fi.Mode(), int64(f.UncompressedSize), io.TeeReader(r, crc))
			ce := r.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
			// The reader exposes zero for an absent member digest. Validate every
			// supplied nonzero member CRC as well as its folder/header checks.
			if f.CRC32 != 0 && crc.Sum32() != f.CRC32 {
				return fmt.Errorf("7z 文件 CRC 校验失败：%s", f.Name)
			}
		}
	case "rar":
		f, e := os.Open(source)
		if e != nil {
			return e
		}
		defer f.Close()
		r, e := rardecode.NewReader(contextReader{ctx, bufio.NewReaderSize(f, 1<<20)})
		if e != nil {
			return e
		}
		for {
			if e = checkpoint(ctx); e != nil {
				return e
			}
			h, e := r.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if h.LinkType != rardecode.LinkTypeNone {
				return fmt.Errorf("不支持 RAR 链接：%s", h.Name)
			}
			if h.Encrypted || h.HeaderEncrypted {
				return fmt.Errorf("暂不支持加密 RAR：%s", h.Name)
			}
			size := h.UnPackedSize
			if h.UnKnownSize {
				size = -1
			}
			if e = s.add(h.Name, h.Mode(), size, r); e != nil {
				return e
			}
		}
	case "tar.gz":
		f, e := os.Open(source)
		if e != nil {
			return e
		}
		defer f.Close()
		gz, e := gzip.NewReader(contextReader{ctx, bufio.NewReaderSize(f, 1<<20)})
		if e != nil {
			return e
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			if e = checkpoint(ctx); e != nil {
				return e
			}
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
			if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
				return fmt.Errorf("不支持 tar 链接或特殊文件：%s", h.Name)
			}
			if e = s.add(h.Name, h.FileInfo().Mode(), h.Size, tr); e != nil {
				return e
			}
		}
		// tar EOF precedes gzip's trailer: drain to verify gzip CRC and truncation.
		if _, e = io.CopyBuffer(io.Discard, contextReader{ctx, gz}, s.buf); e != nil {
			return e
		}
	default:
		return fmt.Errorf("不支持的格式：%s", format)
	}
	if err = checkpoint(ctx); err != nil {
		return err
	}
	from, e := syscall.UTF16PtrFromString(stage)
	if e != nil {
		return e
	}
	to, e := syscall.UTF16PtrFromString(out)
	if e != nil {
		return e
	}
	if err = syscall.MoveFile(from, to); err != nil {
		return err
	}
	s.progress.Total = s.progress.Files
	s.progress.Elapsed = time.Since(s.start)
	if report != nil {
		report(s.progress)
	}
	return nil
}

func validEmpty7z(path string) bool {
	f, e := os.Open(path)
	if e != nil {
		return false
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || s.Size() != 32 {
		return false
	}
	var h [32]byte
	if _, e = io.ReadFull(f, h[:]); e != nil {
		return false
	}
	if string(h[:6]) != "7z\xbc\xaf\x27\x1c" || h[6] != 0 || binary.LittleEndian.Uint32(h[8:12]) != crc32.ChecksumIEEE(h[12:]) {
		return false
	}
	for _, v := range h[12:] {
		if v != 0 {
			return false
		}
	}
	return true
}
