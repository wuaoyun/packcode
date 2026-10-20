package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Optional end-to-end comparison: identical inputs and alternating run order,
// both through CLI executables, to reduce filesystem-cache/order bias.
func TestPerformanceComparison(t *testing.T) {
	if os.Getenv("PACKCODE_COMPARE") != "1" {
		t.Skip("set PACKCODE_COMPARE=1")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.Mkdir(src, 0755)
	for i := 0; i < 1000; i++ {
		p := filepath.Join(src, fmt.Sprintf("dir%d", i/100))
		os.MkdirAll(p, 0755)
		data := []byte(fmt.Sprintf("<?php // file %d\n", i))
		for len(data) < 4096 {
			data = append(data, []byte("function example() { return 'hello world'; }\n")...)
		}
		if e := os.WriteFile(filepath.Join(p, fmt.Sprintf("file%d.php", i)), data, 0644); e != nil {
			t.Fatal(e)
		}
	}
	baseline := os.Getenv("PACKCODE_BASELINE")
	if baseline == "" {
		t.Skip("set PACKCODE_BASELINE to a previous CLI executable")
	}
	old, _ := filepath.Abs(baseline)
	fresh, _ := filepath.Abs("PackCode-cli.exe")
	shared := filepath.Join(root, "input.zip")
	if b, e := exec.Command(old, "-src", src, "-out", shared).CombinedOutput(); e != nil {
		t.Fatalf("fixture: %s %v", b, e)
	}
	for trial := 0; trial < 2; trial++ {
		order := []string{old, fresh}
		if trial%2 == 1 {
			order = []string{fresh, old}
		}
		for _, exe := range order {
			for _, mode := range []string{"store", "deflate", "unpack"} {
				out := filepath.Join(root, fmt.Sprintf("%d-%s-%s", trial, filepath.Base(exe), mode))
				args := []string{"-src", src, "-out", out}
				if mode == "store" {
					args = append(args, "-store")
				}
				if mode == "unpack" {
					args = []string{"-unpack", "-src", shared, "-out", out}
				}
				start := time.Now()
				b, e := exec.Command(exe, args...).CombinedOutput()
				elapsed := time.Since(start)
				if e != nil {
					t.Fatalf("%s: %s %v", exe, b, e)
				}
				t.Logf("trial=%d version=%s mode=%s seconds=%.3f", trial+1, filepath.Base(exe), mode, elapsed.Seconds())
			}
		}
	}
}
