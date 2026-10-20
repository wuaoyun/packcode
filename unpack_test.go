package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func fixtureZIP(t *testing.T, root string, names []string, method uint16) string {
	t.Helper()
	p := filepath.Join(root, "input.zip")
	f, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	for _, name := range names {
		w, e := z.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if e != nil {
			t.Fatal(e)
		}
		if name[len(name)-1] == '/' {
			continue
		}
		if _, e = w.Write([]byte("test contents 中文\n")); e != nil {
			t.Fatal(e)
		}
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestUnpackRoundTrip(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		t.Run(fmt.Sprint(method), func(t *testing.T) {
			root := t.TempDir()
			names := []string{"中文/文件.go", ".env", "empty/"}
			for i := 0; i < 300; i++ {
				names = append(names, fmt.Sprintf("nested/%d.php", i))
			}
			src := fixtureZIP(t, root, names, method)
			out := filepath.Join(root, "out")
			if e := Unpack(context.Background(), src, out, nil); e != nil {
				t.Fatal(e)
			}
			for _, name := range names {
				if name == "empty/" {
					if s, e := os.Stat(filepath.Join(out, name)); e != nil || !s.IsDir() {
						t.Fatal("missing empty dir")
					}
					continue
				}
				b, e := os.ReadFile(filepath.Join(out, name))
				if e != nil || string(b) != "test contents 中文\n" {
					t.Fatalf("%s: %v", name, e)
				}
			}
			if e := Unpack(context.Background(), src, out, nil); e == nil {
				t.Fatal("overwrote existing target")
			}
		})
	}
}

func TestUnpackRejectPaths(t *testing.T) {
	for _, names := range [][]string{{"../escape"}, {"C:/escape"}, {"/absolute"}, {"dir/../../escape"}, {"dir\\..\\escape"}, {"file:ads"}, {"NUL.txt"}, {"dir./file"}, {"A.php", "a.php"}, {"a", "a/b"}} {
		t.Run(fmt.Sprint(names), func(t *testing.T) {
			root := t.TempDir()
			src := fixtureZIP(t, root, names, zip.Store)
			out := filepath.Join(root, "out")
			if e := Unpack(context.Background(), src, out, nil); e == nil {
				t.Fatal("accepted invalid paths")
			}
			paths, _ := filepath.Glob(filepath.Join(root, ".packcode-extract-*"))
			if len(paths) != 0 {
				t.Fatal("staging remains")
			}
		})
	}
}

func TestUnpackCorruptCRC(t *testing.T) {
	root := t.TempDir()
	src := fixtureZIP(t, root, []string{"a.txt"}, zip.Store)
	b, e := os.ReadFile(src)
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	offset, e := z.File[0].DataOffset()
	if e != nil {
		t.Fatal(e)
	}
	b[offset] ^= 0xff
	os.WriteFile(src, b, 0644)
	out := filepath.Join(root, "out")
	if e := Unpack(context.Background(), src, out, nil); !errors.Is(e, zip.ErrChecksum) {
		t.Fatalf("expected CRC error: %v", e)
	}
	if _, e := os.Stat(out); !os.IsNotExist(e) {
		t.Fatal("published corrupt data")
	}
	paths, _ := filepath.Glob(filepath.Join(root, ".packcode-extract-*"))
	if len(paths) != 0 {
		t.Fatal("staging remains")
	}
}

func TestUnpackPauseResumeCancel(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			root := t.TempDir()
			var names []string
			for i := 0; i < 100; i++ {
				names = append(names, fmt.Sprintf("%d.txt", i))
			}
			src := fixtureZIP(t, root, names, zip.Deflate)
			out := filepath.Join(root, "out")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task := &taskContext{Context: ctx}
			paused := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				didPause := false
				done <- Unpack(task, src, out, func(p Progress) {
					if !didPause && p.Files > 0 {
						didPause = true
						task.pause()
						close(paused)
					}
				})
			}()
			select {
			case <-paused:
			case <-time.After(5 * time.Second):
				t.Fatal("pause not reached")
			}
			select {
			case e := <-done:
				t.Fatalf("completed while paused: %v", e)
			case <-time.After(50 * time.Millisecond):
			}
			if abort {
				cancel()
			} else {
				task.pause()
			}
			select {
			case e := <-done:
				if abort {
					if !errors.Is(e, context.Canceled) {
						t.Fatal(e)
					}
				} else if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("deadlock")
			}
			paths, _ := filepath.Glob(filepath.Join(root, ".packcode-extract-*"))
			if len(paths) != 0 {
				t.Fatal("staging remains")
			}
			if !abort {
				b, e := os.ReadFile(filepath.Join(out, "99.txt"))
				if e != nil || len(b) == 0 {
					t.Fatal("missing resumed file")
				}
			}
		})
	}
}

// Compare a serial extraction loop with the persistent worker implementation.
func BenchmarkUnpack(b *testing.B) {
	root := b.TempDir()
	src := filepath.Join(root, "input.zip")
	f, e := os.Create(src)
	if e != nil {
		b.Fatal(e)
	}
	z := zip.NewWriter(f)
	count := 15000
	if n, e := strconv.Atoi(os.Getenv("PACKCODE_BENCH_FILES")); e == nil && n > 0 {
		count = n
	}
	for i := 0; i < count; i++ {
		w, e := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("dir%d/file%d.php", i/100, i), Method: zip.Deflate})
		if e != nil {
			b.Fatal(e)
		}
		w.Write(bytes.Repeat([]byte("<?php echo 'test';\n"), 100))
	}
	z.Close()
	f.Close()
	b.Run("serial", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			out := filepath.Join(root, fmt.Sprintf("serial%d", i))
			z, e := zip.OpenReader(src)
			if e != nil {
				b.Fatal(e)
			}
			for _, en := range z.File {
				p := filepath.Join(out, filepath.FromSlash(en.Name))
				if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
					b.Fatal(e)
				}
				r, e := en.Open()
				if e != nil {
					b.Fatal(e)
				}
				f, e := os.Create(p)
				if e != nil {
					b.Fatal(e)
				}
				_, e = io.Copy(f, r)
				r.Close()
				f.Close()
				if e != nil {
					b.Fatal(e)
				}
			}
			z.Close()
		}
	})
	for _, workers := range []int{1, 2, 4, 8, 16} {
		b.Run(fmt.Sprintf("parallel/%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				out := filepath.Join(root, fmt.Sprintf("parallel%d-%d", workers, i))
				if e := Unpack(context.WithValue(context.Background(), workersKey{}, workers), src, out, nil); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
