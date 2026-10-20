package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	sevenwriter "github.com/go-filesystems/sevenzip"
)

// A 7z header is patched at the start after writing all members. Flush before
// seeking so buffered payload bytes cannot be written over the signature.
type bufferedSeekWriter struct {
	*bufio.Writer
	file *os.File
}

type bufferedSource struct {
	io.Reader
	buffer []byte
}

func (s bufferedSource) WriteTo(w io.Writer) (int64, error) {
	return io.CopyBuffer(w, s.Reader, s.buffer)
}

func (w bufferedSeekWriter) Seek(off int64, whence int) (int64, error) {
	if e := w.Flush(); e != nil {
		return 0, e
	}
	return w.file.Seek(off, whence)
}

func packStream(ctx context.Context, o Options, format string, report func(Progress)) (err error) {
	start := time.Now()
	src, err := filepath.Abs(o.Source)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(o.Output)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("源路径必须是文件夹")
	}
	if _, e := os.Lstat(out); e == nil {
		return fmt.Errorf("输出文件已存在，请换一个文件名")
	} else if !os.IsNotExist(e) {
		return e
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".packcode-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		if e := os.Remove(tmp.Name()); e != nil && !os.IsNotExist(e) {
			err = errors.Join(err, fmt.Errorf("%w：%s：%v", errCleanupFailed, tmp.Name(), e))
		}
	}()
	buffered := bufio.NewWriterSize(tmp, 1<<20)
	var add func(string, fs.FileInfo, string) error
	var finish func() error
	if format == "7z" {
		buf := copyBufferPool.Get().(*[]byte)
		defer copyBufferPool.Put(buf)
		method := sevenwriter.LZMA2
		if o.Store {
			method = sevenwriter.Store
		}
		// Non-solid streams permit direct member reads. A small dictionary avoids
		// allocating a multi-megabyte compressor for every tiny source file.
		z, e := sevenwriter.NewWriter(bufferedSeekWriter{buffered, tmp}, sevenwriter.WithMethod(method), sevenwriter.WithDictionary(64<<10))
		if e != nil {
			return e
		}
		finish = z.Close
		add = func(name string, fi fs.FileInfo, path string) error {
			if fi.IsDir() {
				return z.AddDir(strings.TrimSuffix(name, "/"), fi.Mode().Perm())
			}
			f, e := os.Open(path)
			if e != nil {
				return e
			}
			defer f.Close()
			return z.AddFile(name, fi.Mode().Perm(), fi.Size(), bufferedSource{contextReader{ctx, f}, *buf})
		}
	} else {
		level := gzip.BestSpeed
		if o.Store {
			level = gzip.NoCompression
		}
		gz, e := gzip.NewWriterLevel(buffered, level)
		if e != nil {
			return e
		}
		tw := tar.NewWriter(gz)
		finish = func() error {
			if e := tw.Close(); e != nil {
				return e
			}
			return gz.Close()
		}
		add = func(name string, fi fs.FileInfo, path string) error {
			h, e := tar.FileInfoHeader(fi, "")
			if e != nil {
				return e
			}
			h.Name = name
			if e = tw.WriteHeader(h); e != nil {
				return e
			}
			if fi.IsDir() {
				return nil
			}
			return copyFile(ctx, tw, path)
		}
	}
	p := Progress{}
	lastReport := time.Now()
	// These formats are stream-oriented: scan and write in one pass, with bounded
	// memory. Dotfile/dependency and junction rules match the ZIP source walker.
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = checkpoint(ctx); e != nil {
			return e
		}
		if path == src {
			return nil
		}
		p.Scanned++
		if strings.EqualFold(path, out) || strings.EqualFold(path, tmp.Name()) {
			return nil
		}
		if o.Exclude && (strings.HasPrefix(d.Name(), ".") || d.IsDir() && excluded[strings.ToLower(d.Name())]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, e := d.Info()
		if e != nil {
			return e
		}
		if isReparsePoint(fi) || fi.Mode()&os.ModeSymlink != 0 || (!fi.Mode().IsRegular() && !fi.IsDir()) {
			p.Skipped++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, e := filepath.Rel(src, path)
		if e != nil {
			return e
		}
		name := filepath.ToSlash(rel)
		if fi.IsDir() {
			name += "/"
		}
		if e = add(name, fi, path); e != nil {
			return fmt.Errorf("%s：%w", path, e)
		}
		if !fi.IsDir() {
			p.Files++
			p.Bytes += fi.Size()
		}
		p.Elapsed = time.Since(start)
		if report != nil && (p.Files > 0 || time.Since(lastReport) > 200*time.Millisecond) {
			report(p)
			lastReport = time.Now()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = checkpoint(ctx); err != nil {
		return err
	}
	if err = finish(); err != nil {
		return err
	}
	if err = buffered.Flush(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = checkpoint(ctx); err != nil {
		return err
	}
	if err = os.Link(tmp.Name(), out); err != nil {
		return err
	}
	p.Total = p.Files
	p.Elapsed = time.Since(start)
	if report != nil {
		report(p)
	}
	return nil
}

// Compile-time assertion documents the writer contract used by the 7z encoder.
var _ io.WriteSeeker = bufferedSeekWriter{}
