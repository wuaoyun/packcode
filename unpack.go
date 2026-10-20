package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type extractEntry struct {
	file *zip.File
	name string
}

// Hide os.File.ReadFrom so io.CopyBuffer uses the worker's reusable buffer
// instead of falling back to a fresh allocation for every extracted member.
type writeOnly struct{ io.Writer }

// Unpack extracts into an exclusively created sibling directory. Only after all
// workers finish and ZIP CRC checks pass is it renamed to the requested folder.
// Cancellation removes only that task's staging directory, never existing data.
func Unpack(ctx context.Context, source, destination string, report func(Progress)) (err error) {
	start := time.Now()
	out, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, e := os.Lstat(out); e == nil {
		return fmt.Errorf("目标文件夹已存在，请选择新的文件夹")
	} else if !os.IsNotExist(e) {
		return e
	}
	z, closeArchive, err := openArchive(ctx, source)
	if err != nil {
		return err
	}
	defer closeArchive()
	var files []extractEntry
	dirs := map[string]string{}
	names := map[string]bool{}
	p := Progress{}
	lastReport := time.Now()
	for _, f := range z.File {
		if e := checkpoint(ctx); e != nil {
			return e
		}
		name, e := safeMember(f.Name)
		if e != nil {
			return e
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return fmt.Errorf("不支持的 ZIP 成员类型：%s", f.Name)
		}
		key := strings.ToLower(name)
		if _, ok := names[key]; ok {
			return fmt.Errorf("ZIP 含重复路径：%s", f.Name)
		}
		names[key] = mode.IsDir()
		if mode.IsDir() {
			dirs[key] = name
		} else {
			files = append(files, extractEntry{f, name})
			p.Total++
		}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			dirs[strings.ToLower(parent)] = parent
		}
		p.Scanned++
		p.Elapsed = time.Since(start)
		if report != nil && time.Since(lastReport) > 200*time.Millisecond {
			report(p)
			lastReport = time.Now()
		}
	}
	for key := range dirs {
		if isDir, exists := names[key]; exists && !isDir {
			return fmt.Errorf("ZIP 文件与目录路径冲突：%s", key)
		}
	}
	stage, err := os.MkdirTemp(filepath.Dir(out), ".packcode-extract-*")
	if err != nil {
		return err
	}
	defer func() {
		// Both paths are absolute siblings; validate the generated cleanup target.
		rel, e := filepath.Rel(filepath.Dir(out), stage)
		if e != nil || filepath.Dir(rel) != "." || !strings.HasPrefix(rel, ".packcode-extract-") {
			err = errors.Join(err, fmt.Errorf("%w：清理路径校验失败", errCleanupFailed))
			return
		}
		if e = os.RemoveAll(stage); e != nil {
			err = errors.Join(err, fmt.Errorf("%w：%s：%v", errCleanupFailed, stage, e))
		}
	}()
	// Create each distinct directory once, rather than calling MkdirAll per file.
	for _, dir := range dirs {
		if e := checkpoint(ctx); e != nil {
			return e
		}
		if e := os.MkdirAll(filepath.Join(stage, dir), 0755); e != nil {
			return e
		}
	}
	workers := workerCount(ctx)
	// Creating tiny Windows files is metadata-bound. The measured default of two
	// writers avoids the contention seen at eight; faster disks can opt into more.
	if requested, _ := ctx.Value(workersKey{}).(int); requested <= 0 && workers > 2 {
		workers = 2
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan extractEntry, workers*2)
	type result struct {
		n   int64
		err error
	}
	results := make(chan result, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256<<10)
			for en := range jobs {
				if e := checkpoint(workCtx); e != nil {
					return
				}
				n, e := extractFile(workCtx, stage, en, buf)
				results <- result{n, e}
				if e != nil {
					cancel()
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, en := range files {
			select {
			case jobs <- en:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	// Drain all workers before cleanup: on Windows open files cannot be removed.
	for r := range results {
		if r.err != nil {
			if err == nil || errors.Is(err, context.Canceled) {
				err = r.err
			}
			continue
		}
		p.Files++
		p.Bytes += r.n
		p.Elapsed = time.Since(start)
		if report != nil {
			report(p)
		}
	}
	if err != nil {
		return err
	}
	if err = checkpoint(ctx); err != nil {
		return err
	}
	// MoveFile fails if the destination appeared while extracting (no overwrite).
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
	p.Elapsed = time.Since(start)
	if report != nil {
		report(p)
	}
	return nil
}

func safeMember(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("ZIP 文件名不是 UTF-8，请使用支持 Unicode 文件名的 ZIP：%q", name)
	}
	// Reject traversal, drive paths, ADS, reserved device names and Windows aliases.
	n := strings.ReplaceAll(name, "\\", "/")
	n = strings.TrimSuffix(n, "/")
	if n == "" || strings.ContainsAny(n, ":\x00") {
		return "", fmt.Errorf("ZIP 路径无效：%q", name)
	}
	for _, part := range strings.Split(n, "/") {
		device := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if device == "CON" || device == "PRN" || device == "AUX" || device == "NUL" || device == "CONIN$" || device == "CONOUT$" || ((strings.HasPrefix(device, "COM") || strings.HasPrefix(device, "LPT")) && strings.Contains("|1|2|3|4|5|6|7|8|9|¹|²|³|", "|"+strings.TrimPrefix(strings.TrimPrefix(device, "COM"), "LPT")+"|")) {
			return "", fmt.Errorf("ZIP 含 Windows 保留名称：%q", name)
		}
		for _, ch := range part {
			if ch < 32 {
				return "", fmt.Errorf("ZIP 路径包含控制字符：%q", name)
			}
		}
		if part == "" || part == "." || part == ".." || strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, "<>\"|?*") {
			return "", fmt.Errorf("ZIP 路径无效：%q", name)
		}
	}
	n = filepath.FromSlash(n)
	if !filepath.IsLocal(n) {
		return "", fmt.Errorf("ZIP 路径越界或为保留名称：%q", name)
	}
	return n, nil
}

func extractFile(ctx context.Context, stage string, en extractEntry, buf []byte) (n int64, err error) {
	r, err := en.file.Open()
	if err != nil {
		return 0, fmt.Errorf("%s：%w", en.name, err)
	}
	defer r.Close()
	f, err := os.OpenFile(filepath.Join(stage, en.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	n, err = io.CopyBuffer(writeOnly{f}, contextReader{ctx, r}, buf)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return n, fmt.Errorf("%s：%w", en.name, err)
	}
	// Windows does not preserve Unix execute bits. Avoid a redundant attribute
	// update per member; extracted files stay writable for editing and cleanup.
	return n, nil
}
