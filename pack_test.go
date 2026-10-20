package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Reproduce Laravel public/storage on Windows without symlink privileges.
func TestJunctionSkipped(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "project")
	target := filepath.Join(src, "storage", "app", "public")
	if e := os.MkdirAll(target, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(src, "public"), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("original"), 0644); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(src, "public", "storage")
	if b, e := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput(); e != nil {
		t.Fatalf("create junction: %s: %v", b, e)
	}
	var final Progress
	out := filepath.Join(root, "out.zip")
	if e := Pack(context.Background(), Options{src, out, false, true}, func(p Progress) { final = p }); e != nil {
		t.Fatal(e)
	}
	if final.Skipped != 1 || final.Files != 1 {
		t.Fatalf("unexpected progress: %+v", final)
	}
	z, e := zip.OpenReader(out)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == "public/storage/" || f.Name == "public/storage/keep.txt" {
			t.Fatal("junction was followed")
		}
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	for _, store := range []bool{false, true} {
		t.Run(map[bool]string{false: "deflate", true: "store"}[store], func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "source")
			if e := os.MkdirAll(filepath.Join(src, "node_modules"), 0755); e != nil {
				t.Fatal(e)
			}
			os.Mkdir(filepath.Join(src, "空目录"), 0755)
			want := map[string][]byte{"中文.go": []byte("package main\n"), "large.bin": bytes.Repeat([]byte("abcdefghi"), 1000000)}
			for n, b := range want {
				if e := os.WriteFile(filepath.Join(src, n), b, 0644); e != nil {
					t.Fatal(e)
				}
			}
			os.WriteFile(filepath.Join(src, "node_modules", "ignored"), []byte("x"), 0644)
			out := filepath.Join(src, "output.zip")
			o := Options{src, out, store, true}
			if e := Pack(context.Background(), o, nil); e != nil {
				t.Fatal(e)
			}
			r, e := zip.OpenReader(out)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			seen := 0
			for _, f := range r.File {
				if f.Name == "空目录/" {
					continue
				}
				b, ok := want[f.Name]
				if !ok {
					t.Fatalf("unexpected member %s", f.Name)
				}
				if f.Name == "中文.go" && f.Flags&0x800 == 0 {
					t.Fatal("missing UTF-8 ZIP flag")
				}
				rd, e := f.Open()
				if e != nil {
					t.Fatal(e)
				}
				got, e := io.ReadAll(rd)
				rd.Close()
				if e != nil || !bytes.Equal(b, got) {
					t.Fatalf("roundtrip %s: %v", f.Name, e)
				}
				seen++
			}
			if seen != len(want) {
				t.Fatal("missing files")
			}
			if e := Pack(context.Background(), o, nil); e == nil {
				t.Fatal("must refuse overwrite")
			}
		})
	}
}

func TestCancelled(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := filepath.Join(root, "out.zip")
	if e := Pack(ctx, Options{root, out, false, false}, nil); e == nil {
		t.Fatal("expected cancellation")
	}
	if _, e := os.Stat(out); !os.IsNotExist(e) {
		t.Fatal("published cancelled archive")
	}
}
