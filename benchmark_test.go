package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Both revisions run the same fixture, including directory scan and ZIP close.
func BenchmarkManyFiles(b *testing.B) {
	root := b.TempDir()
	src := filepath.Join(root, "src")
	os.Mkdir(src, 0755)
	count := 3000
	if n, e := strconv.Atoi(os.Getenv("PACKCODE_BENCH_FILES")); e == nil && n > 0 {
		count = n
	}
	for i := 0; i < count; i++ {
		p := filepath.Join(src, fmt.Sprintf("file-%05d.php", i))
		if e := os.WriteFile(p, []byte("<?php\n"+string(make([]byte, 2048))), 0644); e != nil {
			b.Fatal(e)
		}
	}
	for _, store := range []bool{true, false} {
		b.Run(fmt.Sprintf("store=%v", store), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				out := filepath.Join(root, fmt.Sprintf("%v-%d.zip", store, i))
				if e := Pack(context.Background(), Options{src, out, store, false}, nil); e != nil {
					b.Fatal(e)
				}
				os.Remove(out)
			}
		})
	}
}
