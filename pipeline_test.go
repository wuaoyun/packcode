package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// More than one pipeline window, varied payloads, empty files and a streamed
// large member catch premature buffer reuse and ordering mistakes.
func TestPipelineContents(t *testing.T) {
	for _, store := range []bool{false, true} {
		t.Run(fmt.Sprint(store), func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "src")
			os.Mkdir(src, 0755)
			want := map[string][]byte{}
			rng := rand.New(rand.NewSource(42))
			for i := 0; i < 70; i++ {
				size := i * 397
				if i == 25 {
					size = 9 << 20
				}
				b := make([]byte, size)
				rng.Read(b)
				name := fmt.Sprintf("%03d.go", i)
				want[name] = b
				if e := os.WriteFile(filepath.Join(src, name), b, 0644); e != nil {
					t.Fatal(e)
				}
			}
			out := filepath.Join(root, "out.zip")
			if e := Pack(context.Background(), Options{src, out, store, false}, nil); e != nil {
				t.Fatal(e)
			}
			z, e := zip.OpenReader(out)
			if e != nil {
				t.Fatal(e)
			}
			defer z.Close()
			if len(z.File) != len(want) {
				t.Fatal("missing members")
			}
			for _, f := range z.File {
				r, e := f.Open()
				if e != nil {
					t.Fatal(e)
				}
				data, e := io.ReadAll(r)
				r.Close()
				if e != nil || !bytes.Equal(data, want[f.Name]) {
					t.Fatalf("corrupted %s: %v", f.Name, e)
				}
			}
		})
	}
}
