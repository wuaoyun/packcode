# 代码快速打包

## ZIP 解压

勾选“ZIP 解压模式”，点击“浏览”选择 ZIP，设置一个尚不存在的目标文件夹，再点击“开始解压”。解压包含压缩包内全部普通文件（包括点开头文件），打包排除规则不作用于解压。支持普通 Store / Deflate ZIP、ZIP64 和 UTF-8 文件名；不支持加密 ZIP、RAR、7z。

解压默认使用 2 个常驻工作任务，每个任务复用 256 KiB 缓冲，提前创建所需目录。使用只读文件映射按需读取 ZIP，避免逐个小文件触发短磁盘读取；映射不可用时使用最多 16 MiB 的分块缓存。完整校验文件 CRC，全部成功后将本次临时目录改名为目标目录。目标目录已存在时不覆盖。暂停、继续、取消并清理、关闭窗口清理均适用于解压；取消不会删除原 ZIP。

本轮同机 3000 个小文件、同一份 ZIP 的并发对比：1/2/4/8/16 路分别为 1.32/0.94/0.98/1.15/1.25 秒。因此解压默认改为 2 路。界面可选择并发数量 1、2、4、8、16、32；“默认”表示打包最多 8 路、解压最多 2 路。不同磁盘表现不同，并发更多不保证更快。系统缓存、磁盘和杀毒扫描会影响结果，实际项目耗时以实测为准。

```powershell
.\PackCode-cli.exe -unpack -src "E:\backup\project.zip" -out "E:\restored-project"
.\PackCode-cli.exe -unpack -src "E:\backup\project.zip" -out "E:\restored-project" -workers 4
```

双击 `PackCode.exe`，选择源文件夹，填写输出 ZIP 路径，点击“开始打包”。下方信息区可选中复制，也可点击“复制信息”复制完整错误或结果。压缩包可由 Windows 资源管理器、7-Zip、WinRAR 解压。

默认使用快速 Deflate 压缩，默认最多 8 个并行任务读取和处理小文件（含极速模式），界面可调整并发数。使用有界流水线重叠读取、压缩和 ZIP 写入，最多预处理 16 个文件，复用压缩器、读取和输出缓冲，并使用 1 MiB 输出缓冲合并写入；超过 8 MiB 的文件流式处理，避免整个文件进入内存。极速模式使用 ZIP Store，仅打包、不压缩，适合优先考虑耗时的场景。实际速度取决于文件数量、磁盘性能和杀毒软件扫描。

默认排除所有以 `.` 开头的文件和目录（例如 `.env`、`.gitignore`、`.git`、`.github`），以及 `node_modules`、`venv`、`__pycache__` 目录。需要完整归档时取消勾选排除选项。构建输出和其他目录照常打包。空目录保留；符号链接、Windows 目录联接（例如 Laravel 的 public/storage）和特殊文件会跳过，并在完成信息中显示数量；链接目标不通过链接重复打包。普通文件无法读取时仍会报错。打包期间请避免修改源文件。

已有输出文件不会覆盖。临时文件和当前输出文件不会被打进包内。输出目录必须已存在，且所在文件系统须支持硬链接（例如本机 NTFS）；不支持时会明确报错。点击“暂停”保留进度，点击“继续”恢复；暂停在扫描条目、文件读取块和写入条目的边界生效，正在进行的系统读写结束后才会停下。点击“取消并清理”会停止任务并删除本次临时文件，随后可重新开始；不会删除其他任务的临时文件或已完成的 ZIP。正常关闭窗口也会先取消并清理；清理失败时会显示具体临时路径。强制结束进程或断电仍可能留下临时文件。

命令行版本 `PackCode-cli.exe`：

```powershell
.\PackCode-cli.exe -src "E:\my-project" -out "E:\backup\project.zip"
.\PackCode-cli.exe -src "E:\my-project" -out "E:\backup\project.zip" -store -exclude=false
```

构建（Go 1.24+，Windows amd64）：

```powershell
go build -trimpath -ldflags "-s -w -H=windowsgui" -o PackCode.exe .
go build -trimpath -ldflags "-s -w" -o PackCode-cli.exe .
go test ./...
```

本轮新旧 CLI 轮换顺序对比同一组 1000 个约 4 KiB 文件，两轮快速压缩旧版为 0.072/0.059 秒，新版为 0.047/0.045 秒。极速打包和预热后的解压差距不明显；首次解压波动很大，不能作为稳定提速依据。扫描阶段显示条目数量，打包阶段显示已完成/总文件数。

可复现测试：`go test -race ./...`；可选 CLI 对比测试需设置 `PACKCODE_COMPARE=1` 和 `PACKCODE_BASELINE`（旧版 CLI 的绝对路径），运行 `go test -run TestPerformanceComparison -v -count=1`。批量小文件基准可用 `PACKCODE_BENCH_FILES` 调整数量。
