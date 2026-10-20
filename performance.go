package main

import (
	"context"
	"runtime"
)

type workersKey struct{}

// Storage devices differ: extra workers help SSDs but can hurt seek-bound disks.
// Keep a conservative default and allow the same bound for packing/extraction.
func workerCount(ctx context.Context) int {
	n, _ := ctx.Value(workersKey{}).(int)
	if n <= 0 {
		n = runtime.NumCPU()
		if n > 8 {
			n = 8
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 32 {
		n = 32
	}
	return n
}
