package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var createWindow = user32.NewProc("CreateWindowExW")
var sendMessage = user32.NewProc("SendMessageW")
var setText = user32.NewProc("SetWindowTextW")
var sourceBox, outputBox, statusBox, storeBox, excludeBox, startButton uintptr

type uiEvent struct {
	text string
	done bool
}

var uiEvents = make(chan uiEvent, 4)

// Go 1.24 can expose junctions as ModeIrregular rather than ModeSymlink.
// Check the native reparse flag as well, before considering any directory data.
func isReparsePoint(info os.FileInfo) bool {
	a, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && a.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func wide(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
func textOf(h uintptr) string {
	b := make([]uint16, 32768)
	user32.NewProc("GetWindowTextW").Call(h, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	return syscall.UTF16ToString(b)
}
func label(h uintptr, s string) { setText.Call(h, uintptr(unsafe.Pointer(wide(s)))) }
func control(parent uintptr, class, title string, style uintptr, x, y, w, h, id int) uintptr {
	r, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(wide(class))), uintptr(unsafe.Pointer(wide(title))), 0x50000000|style, uintptr(x), uintptr(y), uintptr(w), uintptr(h), parent, uintptr(id), 0, 0)
	font, _, _ := syscall.NewLazyDLL("gdi32.dll").NewProc("GetStockObject").Call(17)
	sendMessage.Call(r, 0x30, font, 1)
	return r
}
func checked(h uintptr) bool { v, _, _ := sendMessage.Call(h, 0xf0, 0, 0); return v == 1 }
func main() {
	src := flag.String("src", "", "源目录（省略则打开窗口）")
	out := flag.String("out", "", "输出压缩包路径，解压时为新建目标目录")
	store := flag.Bool("store", false, "仅打包，不压缩")
	exclude := flag.Bool("exclude", true, "排除点开头的文件/目录及常见依赖目录")
	unpack := flag.Bool("unpack", false, "自动识别并解压 ZIP、7z、RAR、tar.gz")
	format := flag.String("format", "", "打包格式 zip、7z、tar.gz，省略时按输出扩展名识别")
	workers := flag.Int("workers", 0, "并发任务数（1~32，0 使用默认值）")
	licenses := flag.Bool("licenses", false, "显示开源依赖许可证")
	flag.Parse()
	if *licenses {
		fmt.Print(thirdPartyNotices)
		return
	}
	if *src != "" {
		if *out == "" {
			*out = defaultOutput(*src)
			if *format != "" {
				*out = defaultFormatOutput(*src, strings.ToLower(*format))
			}
			if *unpack {
				*out = extractOutput(*src)
			}
		}
		var e error
		runCtx := context.WithValue(context.Background(), workersKey{}, *workers)
		if *unpack {
			e = UnpackArchive(runCtx, *src, *out, nil)
		} else {
			e = PackArchive(runCtx, Options{*src, *out, *store, *exclude}, *format, nil)
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		fmt.Println(*out)
		return
	}
	runWindow()
}
func defaultOutput(src string) string {
	p, _ := filepath.Abs(src)
	return filepath.Join(filepath.Dir(p), filepath.Base(p)+"-"+time.Now().Format("20060102-150405")+".zip")
}

func extractOutput(src string) string {
	p, _ := filepath.Abs(src)
	return stripArchiveExtension(p) + "-解压-" + time.Now().Format("20060102-150405")
}

func defaultFormatOutput(src, format string) string {
	return strings.TrimSuffix(defaultOutput(src), ".zip") + "." + format
}

// The window stays on one OS thread. Worker progress is delivered through a
// channel and consumed by WM_TIMER, so controls are only modified by the UI.
func runWindow() {
	runtime.LockOSThread()
	type windowClass struct {
		Size, Style                        uint32
		Proc                               uintptr
		ClassExtra, WindowExtra            int32
		Instance, Icon, Cursor, Background uintptr
		Menu, Name                         *uint16
		SmallIcon                          uintptr
	}
	name := wide("PackCodeWindow")
	var task *taskContext
	var cancel context.CancelFunc
	var paused, cancelling, closing bool
	var pauseButton, cleanButton uintptr
	var unpackBox, sourceLabel, outputLabel uintptr
	var workersBox uintptr
	var formatBox uintptr
	selectedFormat := func() string {
		n, _, _ := sendMessage.Call(formatBox, 0x147, 0, 0)
		if n < uintptr(len(packingFormats)) {
			return packingFormats[n]
		}
		return "zip"
	}
	enable := func(h uintptr, on bool) {
		var v uintptr
		if on {
			v = 1
		}
		user32.NewProc("EnableWindow").Call(h, v)
	}
	callback := syscall.NewCallback(func(hwnd uintptr, msg uint32, w, l uintptr) uintptr {
		switch msg {
		case 0x10: // WM_CLOSE: wait for cancellation and cleanup before exiting.
			if task != nil {
				closing = true
				cancelling = true
				cancel()
				enable(pauseButton, false)
				enable(cleanButton, false)
				label(statusBox, "正在停止并清理临时文件…")
				return 0
			}
		case 2:
			user32.NewProc("PostQuitMessage").Call(0)
			return 0
		case 0x113:
			for {
				select {
				case event := <-uiEvents:
					if !paused && !cancelling || event.done {
						label(statusBox, event.text)
					}
					if event.done {
						cancel()
						task = nil
						paused = false
						cancelling = false
						enable(startButton, true)
						enable(unpackBox, true)
						enable(workersBox, true)
						enable(formatBox, !checked(unpackBox))
						enable(pauseButton, false)
						enable(cleanButton, false)
						label(pauseButton, "暂停")
						if closing {
							// Keep a cleanup failure visible instead of silently exiting.
							if strings.HasPrefix(event.text, "失败") {
								closing = false
							} else {
								user32.NewProc("DestroyWindow").Call(hwnd)
							}
						}
					}
				default:
					return 0
				}
			}
		case 0x111:
			switch w & 0xffff {
			case 19:
				if task == nil {
					label(statusBox, thirdPartyNotices)
				}
			case 18:
				if w>>16 == 1 && !checked(unpackBox) {
					label(outputBox, stripArchiveExtension(textOf(outputBox))+"."+selectedFormat())
				}
			case 15:
				if checked(unpackBox) {
					label(sourceLabel, "源压缩包文件")
					label(outputLabel, "解压到新文件夹（已有目录不会覆盖）")
					label(startButton, "开始解压")
					enable(storeBox, false)
					enable(excludeBox, false)
					enable(formatBox, false)
					label(outputBox, extractOutput(textOf(sourceBox)))
				} else {
					label(sourceLabel, "源代码文件夹")
					label(outputLabel, "输出压缩包（已有文件不会覆盖）")
					label(startButton, "开始打包")
					enable(storeBox, true)
					enable(excludeBox, true)
					enable(formatBox, true)
					cwd, _ := os.Getwd()
					label(sourceBox, cwd)
					label(outputBox, defaultFormatOutput(cwd, selectedFormat()))
				}
			case 13:
				if task != nil && !cancelling {
					paused = task.pause()
					if paused {
						label(pauseButton, "继续")
						label(statusBox, "已请求暂停，正在进行的读写结束后暂停。点击“继续”恢复，或“取消并清理”删除本次临时文件。")
					} else {
						label(pauseButton, "暂停")
						label(statusBox, "正在继续…")
					}
				}
			case 14:
				if task != nil && !cancelling {
					cancelling = true
					cancel()
					enable(pauseButton, false)
					enable(cleanButton, false)
					label(statusBox, "正在停止并清理本次临时文件…")
				}
			case 12:
				// Read-only EDIT supports native Unicode clipboard copy and selection.
				sendMessage.Call(statusBox, 0xb1, 0, ^uintptr(0)) // EM_SETSEL: select all.
				sendMessage.Call(statusBox, 0x301, 0, 0)          // WM_COPY.
			case 10:
				// Windows' built-in folder dialog avoids requiring an extra UI dependency.
				script := "Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; if ($d.ShowDialog() -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; [Console]::Write($d.SelectedPath) }"
				if checked(unpackBox) {
					script = "Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.OpenFileDialog; $d.Filter = '压缩包|*.zip;*.7z;*.rar;*.tar.gz;*.tgz|所有文件|*.*'; if ($d.ShowDialog() -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; [Console]::Write($d.FileName) }"
				}
				cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script)
				cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
				b, e := cmd.Output()
				if e == nil && len(b) > 0 {
					p := strings.TrimPrefix(string(b), "\ufeff")
					label(sourceBox, p)
					label(outputBox, defaultFormatOutput(p, selectedFormat()))
					if checked(unpackBox) {
						label(outputBox, extractOutput(p))
					}
				}
			case 11:
				if task != nil {
					return 0
				}
				o := Options{textOf(sourceBox), textOf(outputBox), checked(storeBox), checked(excludeBox)}
				isExtract := checked(unpackBox)
				format := selectedFormat()
				if strings.TrimSpace(o.Source) == "" || strings.TrimSpace(o.Output) == "" {
					label(statusBox, "请填写源路径和输出路径")
					return 0
				}
				user32.NewProc("EnableWindow").Call(startButton, 0)
				enable(unpackBox, false)
				enable(workersBox, false)
				enable(formatBox, false)
				selected, _, _ := sendMessage.Call(workersBox, 0x147, 0, 0)
				counts := []int{0, 1, 2, 4, 8, 16, 32}
				count := 0
				if selected < uintptr(len(counts)) {
					count = counts[selected]
				}
				base, stop := context.WithCancel(context.Background())
				cancel = stop
				task = &taskContext{Context: base}
				current := task
				runCtx := context.WithValue(current, workersKey{}, count)
				paused = false
				cancelling = false
				enable(pauseButton, true)
				enable(cleanButton, true)
				label(statusBox, "正在扫描文件…")
				go func() {
					last := time.Now()
					var final Progress
					report := func(p Progress) {
						final = p
						if time.Since(last) < 200*time.Millisecond {
							return
						}
						last = time.Now()
						status := fmt.Sprintf("已打包 %d / %d 个文件 · %.1f MB · %.1f 秒 · 跳过 %d 个链接/特殊文件", p.Files, p.Total, float64(p.Bytes)/(1<<20), p.Elapsed.Seconds(), p.Skipped)
						if isExtract {
							status = fmt.Sprintf("已解压 %d / %d 个文件 · %.1f MB · %.1f 秒", p.Files, p.Total, float64(p.Bytes)/(1<<20), p.Elapsed.Seconds())
						}
						if p.Total == 0 && p.Files > 0 {
							status = fmt.Sprintf("已处理 %d 个文件 · %.1f MB · %.1f 秒", p.Files, float64(p.Bytes)/(1<<20), p.Elapsed.Seconds())
						}
						if p.Files == 0 {
							status = fmt.Sprintf("正在扫描：已检查 %d 个条目 · %.1f 秒", p.Scanned, p.Elapsed.Seconds())
						}
						select {
						case uiEvents <- uiEvent{text: status}:
						default:
						}
					}
					var e error
					if isExtract {
						e = UnpackArchive(runCtx, o.Source, o.Output, report)
					} else {
						e = PackArchive(runCtx, o, format, report)
					}
					s := fmt.Sprintf("完成：%s\r\n共 %d 个文件，耗时 %.1f 秒；跳过 %d 个链接/特殊文件（未打包链接目标）。", o.Output, final.Files, final.Elapsed.Seconds(), final.Skipped)
					if isExtract {
						s = fmt.Sprintf("完成：%s\r\n解压 %d 个文件，耗时 %.1f 秒。", o.Output, final.Files, final.Elapsed.Seconds())
					}
					if e != nil {
						s = "失败：" + e.Error()
						if errors.Is(e, context.Canceled) && !errors.Is(e, errCleanupFailed) {
							s = "已取消：本次任务临时文件已清理。"
						}
					}
					uiEvents <- uiEvent{text: s, done: true}
				}()
			}
			return 0
		}
		r, _, _ := user32.NewProc("DefWindowProcW").Call(hwnd, uintptr(msg), w, l)
		return r
	})
	wc := windowClass{Proc: callback, Background: 6, Name: name}
	wc.Size = uint32(unsafe.Sizeof(wc))
	user32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&wc)))
	hwnd, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(wide("代码极速打包 / 解压"))), 0x10ca0000, 0x80000000, 0x80000000, 740, 430, 0, 0, 0, 0)
	sourceLabel = control(hwnd, "STATIC", "源代码文件夹", 0, 20, 20, 160, 22, 0)
	control(hwnd, "STATIC", "打包格式：", 0, 475, 21, 100, 22, 0)
	formatBox = control(hwnd, "COMBOBOX", "", 3, 575, 17, 125, 160, 18)
	for _, s := range []string{"ZIP（默认）", "7z", "tar.gz"} {
		sendMessage.Call(formatBox, 0x143, 0, uintptr(unsafe.Pointer(wide(s))))
	}
	sendMessage.Call(formatBox, 0x14e, 0, 0)
	cwd, _ := os.Getwd()
	sourceBox = control(hwnd, "EDIT", cwd, 0x00810080, 20, 46, 580, 27, 1)
	control(hwnd, "BUTTON", "浏览…", 0, 610, 45, 90, 29, 10)
	outputLabel = control(hwnd, "STATIC", "输出压缩包（可修改路径；已有文件不会覆盖）", 0, 20, 85, 650, 22, 0)
	outputBox = control(hwnd, "EDIT", defaultOutput(cwd), 0x00810080, 20, 111, 680, 27, 2)
	excludeBox = control(hwnd, "BUTTON", "排除 . 开头的文件/目录及常见依赖目录", 3, 20, 151, 475, 25, 3)
	control(hwnd, "STATIC", "并发：", 0, 510, 155, 65, 22, 0)
	workersBox = control(hwnd, "COMBOBOX", "", 3|0x00200000, 575, 150, 125, 220, 17)
	for _, s := range []string{"默认", "1", "2", "4", "8", "16", "32"} {
		sendMessage.Call(workersBox, 0x143, 0, uintptr(unsafe.Pointer(wide(s))))
	}
	sendMessage.Call(workersBox, 0x14e, 0, 0)
	sendMessage.Call(excludeBox, 0xf1, 1, 0)
	storeBox = control(hwnd, "BUTTON", "极速模式：仅打包，不压缩（文件体积更大）", 3, 20, 182, 440, 25, 4)
	unpackBox = control(hwnd, "BUTTON", "解压模式（自动识别）", 3, 480, 182, 210, 25, 15)
	startButton = control(hwnd, "BUTTON", "开始打包", 0, 20, 223, 130, 34, 11)
	pauseButton = control(hwnd, "BUTTON", "暂停", 0, 160, 223, 90, 34, 13)
	cleanButton = control(hwnd, "BUTTON", "取消并清理", 0, 260, 223, 140, 34, 14)
	enable(pauseButton, false)
	enable(cleanButton, false)
	control(hwnd, "BUTTON", "复制信息", 0, 410, 223, 110, 34, 12)
	control(hwnd, "BUTTON", "开源许可", 0, 540, 223, 160, 34, 19)
	// Multiline, read-only, scrollable text keeps long error paths accessible.
	statusBox = control(hwnd, "EDIT", "就绪 · 支持 PHP / Go / 前端代码。链接和特殊文件会跳过并统计。", 0x00A00844, 20, 267, 680, 110, 5)
	sendMessage.Call(statusBox, 0xc5, 1<<20, 0) // EM_SETLIMITTEXT: allow full license/error text.
	user32.NewProc("SetTimer").Call(hwnd, 1, 100, 0)
	type point struct{ X, Y int32 }
	type message struct {
		Hwnd    uintptr
		Msg     uint32
		W, L    uintptr
		Time    uint32
		Pt      point
		Private uint32
	}
	var m message
	for {
		r, _, _ := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&m)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&m)))
	}
}
