package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtraFormatsRoundTrip(t *testing.T) {
	for _, format := range []string{"7z", "tar.gz"} {
		for _, store := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/store=%v", format, store), func(t *testing.T) {
				root := t.TempDir()
				src := filepath.Join(root, ".source")
				os.MkdirAll(filepath.Join(src, "空目录"), 0755)
				os.MkdirAll(filepath.Join(src, "nested"), 0755)
				os.MkdirAll(filepath.Join(src, "node_modules"), 0755)
				want := map[string][]byte{"00-empty.txt": {}, "nested/中文.go": bytes.Repeat([]byte("package example\n"), 10000), "zz-after-empty.php": []byte("<?php echo 'ok';\n")}
				for name, data := range want {
					if e := os.WriteFile(filepath.Join(src, name), data, 0644); e != nil {
						t.Fatal(e)
					}
				}
				os.WriteFile(filepath.Join(src, ".env"), []byte("secret"), 0644)
				os.WriteFile(filepath.Join(src, "node_modules", "ignored.js"), []byte("ignored"), 0644)
				out := filepath.Join(root, "archive."+format)
				if e := PackArchive(context.Background(), Options{src, out, store, true}, format, nil); e != nil {
					t.Fatal(e)
				}
				// Recognition must use the signature even after a user renames the download.
				renamed := filepath.Join(root, "renamed.data")
				if e := os.Rename(out, renamed); e != nil {
					t.Fatal(e)
				}
				dest := filepath.Join(root, "restored")
				if e := UnpackArchive(context.Background(), renamed, dest, nil); e != nil {
					t.Fatal(e)
				}
				for name, w := range want {
					got, e := os.ReadFile(filepath.Join(dest, name))
					if e != nil || !bytes.Equal(got, w) {
						t.Fatalf("%s: %v", name, e)
					}
				}
				if _, e := os.Stat(filepath.Join(dest, ".env")); !os.IsNotExist(e) {
					t.Fatal("dotfile not excluded")
				}
				if _, e := os.Stat(filepath.Join(dest, "node_modules")); !os.IsNotExist(e) {
					t.Fatal("dependency not excluded")
				}
				if s, e := os.Stat(filepath.Join(dest, "空目录")); e != nil || !s.IsDir() {
					t.Fatal("missing empty directory")
				}
				if e := PackArchive(context.Background(), Options{src, renamed, store, true}, format, nil); e == nil {
					t.Fatal("overwrote output")
				}
				if e := UnpackArchive(context.Background(), renamed, dest, nil); e == nil {
					t.Fatal("overwrote directory")
				}
				if tarExe, e := exec.LookPath("tar.exe"); e == nil {
					check := filepath.Join(root, "external")
					os.Mkdir(check, 0755)
					if b, e := exec.Command(tarExe, "-xf", renamed, "-C", check).CombinedOutput(); e != nil {
						t.Fatalf("libarchive compatibility: %s: %v", b, e)
					}
					for name, w := range want {
						got, e := os.ReadFile(filepath.Join(check, name))
						if e != nil || !bytes.Equal(got, w) {
							t.Fatalf("external content %s: %v", name, e)
						}
					}
				}
			})
		}
	}
}

// A minimal independent RAR 4 stored fixture, including header/file CRCs.
func rar4Fixture(names []string) []byte {
	var out bytes.Buffer
	out.Write([]byte("Rar!\x1a\x07\x00"))
	writeHeader := func(h []byte) { binary.LittleEndian.PutUint16(h, uint16(crc32.ChecksumIEEE(h[2:]))); out.Write(h) }
	main := make([]byte, 13)
	main[2] = 0x73
	binary.LittleEndian.PutUint16(main[5:], 13)
	writeHeader(main)
	for _, name := range names {
		data := []byte("RAR contents: " + name)
		h := make([]byte, 32+len(name))
		h[2] = 0x74
		binary.LittleEndian.PutUint16(h[3:], 0x8000)
		binary.LittleEndian.PutUint16(h[5:], uint16(len(h)))
		binary.LittleEndian.PutUint32(h[7:], uint32(len(data)))
		binary.LittleEndian.PutUint32(h[11:], uint32(len(data)))
		h[15] = 2
		binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(data))
		h[24] = 20
		h[25] = 0x30
		binary.LittleEndian.PutUint16(h[26:], uint16(len(name)))
		binary.LittleEndian.PutUint32(h[28:], 0x20)
		copy(h[32:], name)
		writeHeader(h)
		out.Write(data)
	}
	end := make([]byte, 7)
	end[2] = 0x7b
	binary.LittleEndian.PutUint16(end[5:], 7)
	writeHeader(end)
	return out.Bytes()
}

func decodeUUFixture(t *testing.T, path string) []byte {
	t.Helper()
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var out []byte
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, "begin ") || line == "end" || line == "" {
			continue
		}
		n := int(line[0]-32) & 63
		if n == 0 {
			continue
		}
		var data []byte
		for i := 1; i+3 < len(line); i += 4 {
			a, b, c, d := (line[i]-32)&63, (line[i+1]-32)&63, (line[i+2]-32)&63, (line[i+3]-32)&63
			data = append(data, a<<2|b>>4, b<<4|c>>2, c<<6|d)
		}
		if len(data) < n {
			t.Fatal("truncated uu fixture")
		}
		out = append(out, data[:n]...)
	}
	if e = scanner.Err(); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestRARVersionsAndCRC(t *testing.T) {
	for _, version := range []string{"4", "5"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			data := rar4Fixture([]string{"nested/a.txt", "b.txt"})
			want := map[string][]byte{"nested/a.txt": []byte("RAR contents: nested/a.txt"), "b.txt": []byte("RAR contents: b.txt")}
			if version == "5" {
				data = decodeUUFixture(t, "testdata/rar5-stored.rar.uu")
				want = map[string][]byte{"helloworld.txt": []byte("hello libarchive test suite!\n")}
			}
			src := filepath.Join(root, "source.rar")
			os.WriteFile(src, data, 0644)
			out := filepath.Join(root, "out")
			if e := UnpackArchive(context.Background(), src, out, nil); e != nil {
				t.Fatal(e)
			}
			for name, w := range want {
				got, e := os.ReadFile(filepath.Join(out, name))
				if e != nil || !bytes.Equal(got, w) {
					t.Fatalf("%s: %q %v", name, got, e)
				}
			}
			// Alter a stored payload without altering headers: checksum validation must fail.
			needle := []byte("RAR contents:")
			if version == "5" {
				needle = []byte("hello libarchive")
			}
			offset := bytes.Index(data, needle)
			if offset < 0 {
				t.Fatal("missing payload")
			}
			data[offset] ^= 0xff
			os.WriteFile(src, data, 0644)
			bad := filepath.Join(root, "bad")
			if e := UnpackArchive(context.Background(), src, bad, nil); e == nil {
				t.Fatal("accepted corrupted RAR")
			}
			assertNoTaskFiles(t, root, bad)
		})
	}
}

func assertNoTaskFiles(t *testing.T, root, destination string) {
	t.Helper()
	if _, e := os.Stat(destination); !os.IsNotExist(e) {
		t.Fatal("published partial destination")
	}
	paths, _ := filepath.Glob(filepath.Join(root, ".packcode-*"))
	if len(paths) > 0 {
		t.Fatalf("task files remain: %v", paths)
	}
}

func TestExtraFormatPauseResumeCleanup(t *testing.T) {
	for _, format := range []string{"7z", "tar.gz"} {
		for _, extract := range []bool{false, true} {
			for _, abort := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/extract=%v/cancel=%v", format, extract, abort), func(t *testing.T) {
					root := t.TempDir()
					src := filepath.Join(root, "src")
					os.Mkdir(src, 0755)
					for i := 0; i < 6; i++ {
						os.WriteFile(filepath.Join(src, fmt.Sprint(i)), []byte("contents"), 0644)
					}
					archive := filepath.Join(root, "archive."+format)
					if extract {
						if e := PackArchive(context.Background(), Options{src, archive, true, false}, format, nil); e != nil {
							t.Fatal(e)
						}
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					task := &taskContext{Context: ctx}
					paused := make(chan struct{})
					done := make(chan error, 1)
					output := archive
					if extract {
						output = filepath.Join(root, "out")
					}
					go func() {
						didPause := false
						report := func(p Progress) {
							if !didPause && p.Files > 0 {
								didPause = true
								task.pause()
								close(paused)
							}
						}
						if extract {
							done <- UnpackArchive(task, archive, output, report)
						} else {
							done <- PackArchive(task, Options{src, output, true, false}, format, report)
						}
					}()
					select {
					case <-paused:
					case <-time.After(5 * time.Second):
						t.Fatal("pause not reached")
					}
					select {
					case e := <-done:
						t.Fatalf("completed while paused: %v", e)
					case <-time.After(30 * time.Millisecond):
					}
					if abort {
						cancel()
					} else {
						task.pause()
					}
					select {
					case e := <-done:
						if abort && !errors.Is(e, context.Canceled) {
							t.Fatal(e)
						}
						if !abort && e != nil {
							t.Fatal(e)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("worker blocked")
					}
					if abort {
						assertNoTaskFiles(t, root, output)
					}
				})
			}
		}
	}
}

func TestTarPathsAndIntegrity(t *testing.T) {
	for _, name := range []string{"../escape", "C:/escape", "dir/../../escape", "file:ads"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "bad.tar.gz")
			f, _ := os.Create(src)
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: 1})
			tw.Write([]byte("x"))
			tw.Close()
			gz.Close()
			f.Close()
			out := filepath.Join(root, "out")
			if e := UnpackArchive(context.Background(), src, out, nil); e == nil {
				t.Fatal("accepted traversal")
			}
			assertNoTaskFiles(t, root, out)
		})
	}
	root := t.TempDir()
	src := filepath.Join(root, "bad.tar.gz")
	f, _ := os.Create(src)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "./", Mode: 0755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "./file.txt", Mode: 0644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	f.Close()
	if e := UnpackArchive(context.Background(), src, filepath.Join(root, "valid"), nil); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(src)
	data[len(data)-8] ^= 0xff
	os.WriteFile(src, data, 0644)
	out := filepath.Join(root, "out")
	if e := UnpackArchive(context.Background(), src, out, nil); e == nil {
		t.Fatal("gzip CRC not checked")
	}
	assertNoTaskFiles(t, root, out)
}

func Test7zCorruptPayload(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.Mkdir(src, 0755)
	payload := []byte("unique stored payload for CRC check")
	os.WriteFile(filepath.Join(src, "a.txt"), payload, 0644)
	out := filepath.Join(root, "archive.7z")
	if e := PackArchive(context.Background(), Options{src, out, true, false}, "7z", nil); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(out)
	off := bytes.Index(data, payload)
	if off < 0 {
		t.Fatal("missing stored payload")
	}
	data[off] ^= 0xff
	os.WriteFile(out, data, 0644)
	dest := filepath.Join(root, "out")
	if e := UnpackArchive(context.Background(), out, dest, nil); e == nil {
		t.Fatal("7z CRC not checked")
	}
	assertNoTaskFiles(t, root, dest)
}

// Ensure the generic streaming sink rejects links even before opening any path.
func TestStreamRejectLinksAndConflicts(t *testing.T) {
	root := t.TempDir()
	s := &extractionSession{ctx: context.Background(), stage: root, seen: map[string]bool{}, dirs: map[string]bool{}, buf: make([]byte, 1024), start: time.Now()}
	if e := s.add("link", os.ModeSymlink, 1, strings.NewReader("x")); e == nil {
		t.Fatal("accepted link")
	}
	if e := s.add("file", 0644, 1, strings.NewReader("x")); e != nil {
		t.Fatal(e)
	}
	if e := s.add("file/child", 0644, 1, strings.NewReader("x")); e == nil {
		t.Fatal("accepted conflicting parent")
	}
}

func TestExtraEmptyArchivesAndFullBackup(t *testing.T) {
	for _, format := range []string{"7z", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "src")
			os.Mkdir(src, 0755)
			os.WriteFile(filepath.Join(src, ".env"), []byte("configuration"), 0644)
			for _, exclude := range []bool{true, false} {
				out := filepath.Join(root, fmt.Sprintf("%v.%s", exclude, format))
				if e := PackArchive(context.Background(), Options{src, out, false, exclude}, format, nil); e != nil {
					t.Fatal(e)
				}
				dest := filepath.Join(root, fmt.Sprintf("restore-%v", exclude))
				if e := UnpackArchive(context.Background(), out, dest, nil); e != nil {
					t.Fatal(e)
				}
				data, e := os.ReadFile(filepath.Join(dest, ".env"))
				if exclude {
					if !os.IsNotExist(e) {
						t.Fatal("excluded file restored")
					}
				} else if e != nil || string(data) != "configuration" {
					t.Fatal("full backup missing dotfile", e)
				}
			}
		})
	}
}

func TestExternal7zInput(t *testing.T) {
	tarExe, e := exec.LookPath("tar.exe")
	if e != nil {
		t.Skip("libarchive tar unavailable")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.Mkdir(src, 0755)
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(src, fmt.Sprint(i)), []byte(fmt.Sprintf("solid input %d", i)), 0644)
	}
	archive := filepath.Join(root, "external.7z")
	if b, e := exec.Command(tarExe, "--format=7zip", "-cf", archive, "-C", src, ".").CombinedOutput(); e != nil {
		t.Fatalf("external fixture: %s: %v", b, e)
	}
	out := filepath.Join(root, "out")
	if e := UnpackArchive(context.Background(), archive, out, nil); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 10; i++ {
		data, e := os.ReadFile(filepath.Join(out, fmt.Sprint(i)))
		if e != nil || string(data) != fmt.Sprintf("solid input %d", i) {
			t.Fatal("external 7z content mismatch", e)
		}
	}
}

func TestRARCompressedAndSolidInput(t *testing.T) {
	tarExe, e := exec.LookPath("tar.exe")
	if e != nil {
		t.Skip("independent libarchive decoder unavailable")
	}
	for _, fixture := range []string{"rar5-compressed.rar.uu", "rar5-solid.rar.uu"} {
		t.Run(fixture, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "input.rar")
			os.WriteFile(source, decodeUUFixture(t, filepath.Join("testdata", fixture)), 0644)
			ours := filepath.Join(root, "ours")
			if e := UnpackArchive(context.Background(), source, ours, nil); e != nil {
				t.Fatal(e)
			}
			reference := filepath.Join(root, "reference")
			os.Mkdir(reference, 0755)
			if b, e := exec.Command(tarExe, "-xf", source, "-C", reference).CombinedOutput(); e != nil {
				t.Fatalf("libarchive decode: %s: %v", b, e)
			}
			count := 0
			if e := filepath.WalkDir(reference, func(path string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if d.IsDir() {
					return nil
				}
				rel, e := filepath.Rel(reference, path)
				if e != nil {
					return e
				}
				want, e := os.ReadFile(path)
				if e != nil {
					return e
				}
				got, e := os.ReadFile(filepath.Join(ours, rel))
				if e != nil {
					return e
				}
				if !bytes.Equal(got, want) {
					return fmt.Errorf("contents differ: %s", rel)
				}
				count++
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			if count == 0 {
				t.Fatal("fixture contains no files")
			}
		})
	}
}

func TestRARCancelWhilePaused(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "input.rar")
	os.WriteFile(source, rar4Fixture([]string{"a", "b", "c"}), 0644)
	out := filepath.Join(root, "out")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := &taskContext{Context: ctx}
	paused := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		didPause := false
		done <- UnpackArchive(task, source, out, func(p Progress) {
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
		t.Fatal("RAR pause not reached")
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RAR cancellation blocked")
	}
	assertNoTaskFiles(t, root, out)
}

// Opt-in smoke test for the actual distribution executable, including flags,
// auto-detection, filename encoding and the embedded license notices.
func TestBuiltCLI(t *testing.T) {
	exe := os.Getenv("PACKCODE_CLI")
	if exe == "" {
		t.Skip("set PACKCODE_CLI to a built CLI executable")
	}
	root := t.TempDir()
	src := filepath.Join(root, "source")
	os.Mkdir(src, 0755)
	os.WriteFile(filepath.Join(src, "中文.go"), []byte("package smoke"), 0644)
	os.WriteFile(filepath.Join(src, ".env"), []byte("excluded"), 0644)
	run := func(args ...string) {
		t.Helper()
		if b, e := exec.Command(exe, args...).CombinedOutput(); e != nil {
			t.Fatalf("CLI %v: %s: %v", args, b, e)
		}
	}
	for _, format := range []string{"zip", "7z", "tar.gz"} {
		archive := filepath.Join(root, "archive."+format)
		run("-src", src, "-out", archive, "-format", format)
		dest := filepath.Join(root, "out-"+format)
		run("-unpack", "-src", archive, "-out", dest)
		data, e := os.ReadFile(filepath.Join(dest, "中文.go"))
		if e != nil || string(data) != "package smoke" {
			t.Fatal("CLI roundtrip mismatch", e)
		}
		if _, e := os.Stat(filepath.Join(dest, ".env")); !os.IsNotExist(e) {
			t.Fatal("CLI exclusion failed")
		}
		t.Logf("PASS %s", format)
	}
	rar := filepath.Join(root, "input.rar")
	os.WriteFile(rar, decodeUUFixture(t, "testdata/rar5-stored.rar.uu"), 0644)
	rarOut := filepath.Join(root, "rar-out")
	run("-unpack", "-src", rar, "-out", rarOut)
	data, e := os.ReadFile(filepath.Join(rarOut, "helloworld.txt"))
	if e != nil || string(data) != "hello libarchive test suite!\n" {
		t.Fatal("CLI RAR decode mismatch", e)
	}
	notices, e := exec.Command(exe, "-licenses").Output()
	if e != nil || !bytes.Contains(notices, []byte("github.com/go-filesystems/sevenzip")) {
		t.Fatal("CLI notices missing", e)
	}
}
