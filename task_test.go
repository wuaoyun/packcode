package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPauseResumeAndCancelCleanup(t *testing.T) {
	for _, store := range []bool{false, true} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("store=%v/cancel=%v", store, abort), func(t *testing.T) {
				root := t.TempDir()
				src := filepath.Join(root, "src")
				if e := os.Mkdir(src, 0755); e != nil {
					t.Fatal(e)
				}
				for i := 0; i < 30; i++ {
					if e := os.WriteFile(filepath.Join(src, fmt.Sprintf("%d.go", i)), []byte("package main\n"), 0644); e != nil {
						t.Fatal(e)
					}
				}
				// A different task's temporary file must never be deleted by this task.
				other := filepath.Join(root, ".packcode-other.tmp")
				os.WriteFile(other, []byte("keep"), 0644)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				task := &taskContext{Context: ctx}
				paused := make(chan struct{})
				done := make(chan error, 1)
				out := filepath.Join(root, "out.zip")
				go func() {
					didPause := false
					done <- Pack(context.WithValue(task, workersKey{}, 2), Options{src, out, store, false}, func(p Progress) {
						if !didPause && p.Files == 1 {
							didPause = true
							task.pause()
							close(paused)
						}
					})
				}()
				select {
				case <-paused:
				case <-time.After(5 * time.Second):
					t.Fatal("never reached pause")
				}
				select {
				case e := <-done:
					t.Fatalf("finished while paused: %v", e)
				case <-time.After(50 * time.Millisecond):
				}
				if abort {
					cancel()
				} else {
					task.pause()
				}
				var result error
				select {
				case result = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not wake")
				}
				if abort {
					if !errors.Is(result, context.Canceled) {
						t.Fatalf("cancel: %v", result)
					}
					if _, e := os.Stat(out); !os.IsNotExist(e) {
						t.Fatal("published cancelled ZIP")
					}
				} else {
					if result != nil {
						t.Fatal(result)
					}
					z, e := zip.OpenReader(out)
					if e != nil {
						t.Fatal(e)
					}
					if len(z.File) != 30 {
						t.Fatal("missing resumed files")
					}
					z.Close()
				}
				paths, e := filepath.Glob(filepath.Join(root, ".packcode-*.tmp"))
				if e != nil {
					t.Fatal(e)
				}
				if len(paths) != 1 || paths[0] != other {
					t.Fatalf("unexpected temporary files: %v", paths)
				}
			})
		}
	}
}

func TestCancelWhileScanPaused(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	task := &taskContext{Context: ctx}
	task.pause()
	done := make(chan error, 1)
	go func() { done <- Pack(task, Options{root, filepath.Join(root, "out.zip"), true, false}, nil) }()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan cancellation blocked")
	}
	files, _ := os.ReadDir(root)
	if len(files) != 0 {
		t.Fatal("temporary file remains")
	}
}
