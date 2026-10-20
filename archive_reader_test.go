package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestArchiveCacheReadAt(t *testing.T) {
	data := make([]byte, 18*archiveBlockSize+17)
	for i := range data {
		data[i] = byte(i*31 + i/1021)
	}
	path := filepath.Join(t.TempDir(), "data")
	if e := os.WriteFile(path, data, 0644); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := &archiveReader{file: f}
	var wg sync.WaitGroup
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for _, off := range []int64{0, archiveBlockSize - 7, int64(k) * archiveBlockSize, 17 * archiveBlockSize, int64(len(data)) - 5, int64(len(data)) + 1} {
				got, want := make([]byte, 100), make([]byte, 100)
				n, e := r.ReadAt(got, off)
				wn, we := bytes.NewReader(data).ReadAt(want, off)
				if n != wn || !errors.Is(e, we) || !bytes.Equal(got[:n], want[:wn]) {
					t.Errorf("offset %d: n=%d err=%v, want n=%d err=%v", off, n, e, wn, we)
				}
			}
		}(k)
	}
	wg.Wait()
	if n, e := r.ReadAt(nil, 0); n != 0 || e != nil {
		t.Fatal(n, e)
	}
	if _, e := r.ReadAt(make([]byte, 1), -1); e == nil {
		t.Fatal("negative offset accepted")
	}
}

func TestUnpackLargeMappedFile(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "large.zip")
	f, e := os.Create(src)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	w, e := z.CreateHeader(&zip.FileHeader{Name: "large.bin", Method: zip.Store})
	if e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), 600000)
	if _, e = w.Write(data); e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	f.Close()
	out := filepath.Join(root, "out")
	if e = Unpack(context.Background(), src, out, nil); e != nil {
		t.Fatal(e)
	}
	rd, e := os.Open(filepath.Join(out, "large.bin"))
	if e != nil {
		t.Fatal(e)
	}
	defer rd.Close()
	got, e := io.ReadAll(rd)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("large extraction mismatch", e)
	}
}
