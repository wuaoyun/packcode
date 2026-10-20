package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var packingFormats = []string{"zip", "7z", "tar.gz"}

func archiveFormat(path string) string {
	lower := strings.ToLower(path)
	for _, suffix := range []string{".tar.gz", ".tgz", ".zip", ".7z", ".rar"} {
		if strings.HasSuffix(lower, suffix) {
			if suffix == ".tgz" {
				return "tar.gz"
			}
			return strings.TrimPrefix(suffix, ".")
		}
	}
	return ""
}

func stripArchiveExtension(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".tgz") {
		return path[:len(path)-4]
	}
	if f := archiveFormat(path); f != "" {
		return path[:len(path)-len(f)-1]
	}
	return strings.TrimSuffix(path, filepath.Ext(path))
}

func PackArchive(ctx context.Context, o Options, format string, report func(Progress)) error {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "tgz" {
		format = "tar.gz"
	}
	if format == "" {
		format = archiveFormat(o.Output)
	}
	if extension := archiveFormat(o.Output); extension != "" && format != extension {
		return fmt.Errorf("输出扩展名与打包格式不一致：%s / %s", extension, format)
	}
	switch strings.ToLower(format) {
	case "zip":
		return Pack(ctx, o, report)
	case "7z", "tar.gz":
		return packStream(ctx, o, strings.ToLower(format), report)
	case "rar":
		return fmt.Errorf("RAR 仅支持解压，请选择 ZIP、7z 或 tar.gz 打包")
	default:
		return fmt.Errorf("不支持的打包格式：%s", format)
	}
}

// Dispatch by signature, not just the extension: renamed downloads still open.
// Only the supported archive families are admitted; encrypted archives report an
// explicit decoder error rather than producing a partial destination directory.
func UnpackArchive(ctx context.Context, source, destination string, report func(Progress)) error {
	f, e := os.Open(source)
	if e != nil {
		return e
	}
	var header [8]byte
	n, _ := f.Read(header[:])
	f.Close()
	kind := ""
	switch {
	case n >= 4 && string(header[:2]) == "PK":
		kind = "zip"
	case n >= 6 && string(header[:6]) == "7z\xbc\xaf\x27\x1c":
		kind = "7z"
	case n >= 7 && string(header[:7]) == "Rar!\x1a\x07\x00":
		kind = "rar"
	case n >= 8 && string(header[:8]) == "Rar!\x1a\x07\x01\x00":
		kind = "rar"
	case n >= 2 && header[0] == 0x1f && header[1] == 0x8b:
		kind = "tar.gz"
	default:
		return fmt.Errorf("无法识别压缩包；支持 ZIP、7z、RAR、tar.gz（含 tgz）")
	}
	if kind == "zip" {
		return Unpack(ctx, source, destination, report)
	}
	return unpackStream(ctx, source, destination, kind, report)
}
