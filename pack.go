package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Source, Output string
	Store, Exclude bool
}
type Progress struct {
	Files   int
	Skipped int
	Scanned int
	Total   int
	Bytes   int64
	Elapsed time.Duration
}
type entry struct {
	path   string
	header *zip.FileHeader
	size   int64
}
type compressed struct {
	data   []byte
	err    error
	buffer *bytes.Buffer
}

var errCleanupFailed = errors.New("临时文件清理失败")

var excluded = map[string]bool{".git": true, ".svn": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".idea": true, ".vscode": true}

// Reuse the sizeable Deflate state instead of allocating one compressor per file.
var compressorPool = sync.Pool{New: func() any { w, _ := flate.NewWriter(io.Discard, flate.BestSpeed); return w }}
var copyBufferPool = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}
var outputBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func releaseOutput(b *bytes.Buffer) {
	if b != nil && b.Cap() <= 1<<20 {
		b.Reset()
		outputBufferPool.Put(b)
	}
}

// Pack writes to a temporary file first. A failed run never leaves a partial archive
// at the requested output path, and an existing archive is never overwritten.
func Pack(ctx context.Context, o Options, report func(Progress)) (err error) {
	start := time.Now()
	src, err := filepath.Abs(o.Source)
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
	out, err := filepath.Abs(o.Output)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(out); err == nil {
		return fmt.Errorf("输出文件已存在，请换一个文件名")
	} else if !os.IsNotExist(err) {
		return err
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
	// ZIP produces many tiny headers. Coalesce these writes, especially in Store
	// mode where unbuffered headers and payloads otherwise each hit the filesystem.
	buffered := bufio.NewWriterSize(tmp, 1<<20)
	zw := zip.NewWriter(buffered)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, flate.BestSpeed) })
	var entries []entry
	progress := Progress{}
	lastScanReport := time.Now()
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := checkpoint(ctx); err != nil {
			return err
		}
		if p == src {
			return nil
		}
		progress.Scanned++
		if report != nil && time.Since(lastScanReport) >= 200*time.Millisecond {
			progress.Elapsed = time.Since(start)
			report(progress)
			lastScanReport = time.Now()
		}
		if strings.EqualFold(p, out) || strings.EqualFold(p, tmp.Name()) {
			return nil
		}
		// Dotfiles and dot-directories are omitted by the optional exclusion rule.
		// SkipDir prevents scanning their contents, which also saves small-file I/O.
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
		// PHP's public/storage often is a Windows junction. Do not follow links:
		// their targets may already be in the tree, outside it, or form a cycle.
		// Real source files remain included; unsupported special entries are counted.
		if isReparsePoint(fi) || fi.Mode()&os.ModeSymlink != 0 || (!fi.Mode().IsRegular() && !fi.IsDir()) {
			progress.Skipped++
			if report != nil {
				report(progress)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		h, e := zip.FileInfoHeader(fi)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		h.Name = filepath.ToSlash(rel)
		h.Method = zip.Deflate
		if o.Store || d.IsDir() {
			h.Method = zip.Store
		}
		if d.IsDir() {
			h.Name += "/"
		}
		entries = append(entries, entry{p, h, fi.Size()})
		if !fi.IsDir() {
			progress.Total++
		}
		return nil
	})
	if err != nil {
		return err
	}
	workers := workerCount(ctx)
	// Persistent workers avoid creating 150,000 goroutines for a large source tree.
	type prepareJob struct {
		en     entry
		result chan compressed
	}
	workCtx, cancel := context.WithCancel(ctx)
	window := workers * 2
	if window > 16 {
		window = 16
	} // Bound lookahead independently of the user-selected worker count.
	jobs := make(chan prepareJob, window)
	ready := make([]chan compressed, window)
	for i := range ready {
		ready[i] = make(chan compressed, 1)
	}
	var workerWG sync.WaitGroup
	for k := 0; k < workers; k++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for job := range jobs {
				job.result <- compressEntry(workCtx, job.en)
			}
		}()
	}
	defer func() { cancel(); close(jobs); workerWG.Wait() }()
	// A bounded ring overlaps reads/compression with ZIP output. Unlike batch
	// barriers, one slow file does not stop all other workers between each batch.
	// At most window small-file buffers exist; large files still stream directly.
	submit := func(i int) {
		en := entries[i]
		ch := ready[i%window]
		if en.header.FileInfo().IsDir() || en.size > 8<<20 {
			ch <- compressed{}
		} else {
			jobs <- prepareJob{en, ch}
		}
	}
	for i := 0; i < window && i < len(entries); i++ {
		submit(i)
	}
	for j := 0; j < len(entries); j++ {
		if err := checkpoint(ctx); err != nil {
			return err
		}
		if e := checkpoint(ctx); e != nil {
			return e
		}
		en := entries[j]
		r := <-ready[j%window]
		if r.err != nil {
			return fmt.Errorf("%s: %w", en.path, r.err)
		}
		if !en.header.FileInfo().IsDir() && en.size <= 8<<20 {
			w, e := zw.CreateRaw(en.header)
			if e != nil {
				return e
			}
			if _, e = w.Write(r.data); e != nil {
				releaseOutput(r.buffer)
				return e
			}
			releaseOutput(r.buffer)
		} else {
			w, e := zw.CreateHeader(en.header)
			if e != nil {
				return e
			}
			if !en.header.FileInfo().IsDir() {
				if e = copyFile(ctx, w, en.path); e != nil {
					return e
				}
			}
		}
		if !en.header.FileInfo().IsDir() {
			progress.Files++
			progress.Bytes += en.size
		}
		progress.Elapsed = time.Since(start)
		if report != nil {
			report(progress)
		}
		if j+window < len(entries) {
			submit(j + window)
		}
	}
	if err = checkpoint(ctx); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = buffered.Flush(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Hard-link publication is atomic and fails if the destination appeared while
	// compressing. The temporary file lives on the same volume as the destination.
	if err = checkpoint(ctx); err != nil {
		return err
	}
	if err = os.Link(tmp.Name(), out); err != nil {
		return fmt.Errorf("保存压缩包失败（目标需支持硬链接，且文件不能已存在）: %w", err)
	}
	progress.Elapsed = time.Since(start)
	if report != nil {
		report(progress)
	}
	return nil
}

func copyFile(ctx context.Context, w io.Writer, path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	buf := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(buf)
	_, e = io.CopyBuffer(w, contextReader{ctx, f}, *buf)
	return e
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := checkpoint(r.ctx); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func compressEntry(ctx context.Context, en entry) compressed {
	if e := checkpoint(ctx); e != nil {
		return compressed{err: e}
	}
	// Workers read and checksum files concurrently in BOTH modes. Store previously
	// did every open/read serially. Emit raw ZIP members without a temporary ZIP
	// writer/reader pair for each file. The outer writer still handles ZIP64.
	b := outputBufferPool.Get().(*bytes.Buffer)
	b.Reset()
	keep := false
	defer func() {
		if !keep {
			releaseOutput(b)
		}
	}()
	f, e := os.Open(en.path)
	if e != nil {
		return compressed{err: e}
	}
	defer f.Close()
	crc := crc32.NewIEEE()
	var dst io.Writer = b
	if en.header.Method == zip.Deflate {
		w := compressorPool.Get().(*flate.Writer)
		defer func() { w.Reset(io.Discard); compressorPool.Put(w) }()
		w.Reset(b)
		dst = w
	}
	// Limit buffers even if a source file grows after the directory scan.
	buf := copyBufferPool.Get().(*[]byte)
	defer copyBufferPool.Put(buf)
	n, e := io.CopyBuffer(io.MultiWriter(dst, crc), io.LimitReader(contextReader{ctx, f}, (8<<20)+1), *buf)
	if e != nil {
		return compressed{err: e}
	}
	if n > 8<<20 {
		return compressed{err: fmt.Errorf("文件在打包期间变大，请停止修改后重试")}
	}
	if w, ok := dst.(*flate.Writer); ok {
		if e = w.Close(); e != nil {
			return compressed{err: e}
		}
	}
	en.header.CRC32 = crc.Sum32()
	en.header.CompressedSize64 = uint64(b.Len())
	en.header.UncompressedSize64 = uint64(n)
	// Raw members carry known sizes and CRC; no descriptor is required.
	en.header.Flags &^= 8
	// CreateRaw does not infer UTF-8 like CreateHeader does. Mark it explicitly
	// so Windows Explorer displays Chinese source paths correctly.
	en.header.Flags |= 0x800
	keep = true
	return compressed{data: b.Bytes(), buffer: b}
}
