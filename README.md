# MegaFolderDownloader — MEGA 文件夹高速下载器

**免账号 · 免安装 · 多连接绕限速 · 字节级断点续传 · 自动代理 · 509 限流自愈**
**No account · No install · Beats per-connection throttling · Byte-level resume · Auto proxy · Self-heals on HTTP 509**

[![Release](https://img.shields.io/github/v/release/unclesimen/mega-folder-downloader)](https://github.com/unclesimen/mega-folder-downloader/releases)
[![Downloads](https://img.shields.io/github/downloads/unclesimen/mega-folder-downloader/total)](https://github.com/unclesimen/mega-folder-downloader/releases)
![Platform](https://img.shields.io/badge/platform-Windows%20x86--64-blue)
![Go](https://img.shields.io/badge/Go-1.23%2B-00ADD8)

一个专为 Windows 打造的 MEGA 公开文件夹下载工具。把文件夹链接粘贴进来，整个文件夹就会按原目录结构下载到本地。它针对 MEGA 的限速机制做了三件事：多文件并行 + 大文件分段并行，把速度叠满；字节级断点续传，中断零损失；遇到 509 配额限流时自动暂停、恢复后自动续传。再大的文件夹，也能挂着安心下完。

A Windows tool built for downloading public MEGA folders. Paste a folder link, and the entire folder lands on your disk with its original directory structure. It is designed around MEGA's throttling: parallel files plus segmented large files stack up the speed, byte-level resume makes interruptions cost nothing, and when the transfer quota runs out (HTTP 509) it pauses and continues on its own once the quota refills. Even huge folders finish while you do something else.

---

## 下载 / Download

到本仓库的 **Releases** 页面下载 `MegaFolderDownloader.exe`（Windows x86-64，无需安装，双击即用）。

Download `MegaFolderDownloader.exe` from the **Releases** page of this repository (Windows x86-64, no installation needed).

## 功能特点 / Features

- **多连接并行**：MEGA 对单条连接限速，本工具同时开多条连接（多文件并行 + 大文件分段并行），把速度叠起来。
  **Parallel connections** — MEGA throttles each connection individually; this tool opens many at once (multiple files, plus segments of large files) so the speeds add up.
- **字节级断点续传**：中断、关机、被限流都不丢进度，重跑同一命令从断点继续。
  **Byte-level resume** — interrupted downloads continue exactly where they stopped.
- **自动代理**：自动读取 Windows 系统代理（与浏览器同款），也支持 `-proxy` 手动指定（http / socks5）。
  **Automatic proxy support** — reads the Windows system proxy (the same one your browser uses); manual `-proxy` flag for http / socks5.
- **限流自愈**：遇到 509（配额用完）自动暂停等待，配额恢复后自动继续，无需守着。
  **Self-healing under quota limits** — on HTTP 509 it pauses and resumes automatically when the quota refills.
- **无需账号、零依赖**：仅支持公开文件夹链接；纯 Go 标准库实现，单个 exe 运行。
  **No account, no dependencies** — public folder links only; single exe built with the Go standard library only.

## 使用方法 / Usage

### 双击运行（最简单）/ Double-click

双击 `MegaFolderDownloader.exe` → 粘贴 MEGA 文件夹链接 → 输入保存目录（回车 = 当前目录下的 `downloads` 文件夹）。

Double-click the exe, paste the MEGA folder link, then enter a target directory (Enter = a `downloads` folder next to the exe).

### 命令行 / Command line

```bat
MegaFolderDownloader.exe "https://mega.nz/folder/xxxx#yyyy" -o D:\Downloads
```

参数可写在链接前或链接后。Flags may be placed before or after the URL.

| 参数 / Flag | 说明 / Description | 默认 / Default |
|---|---|---|
| `-list` | 只列出文件清单，不下载 / List files only | 关 / off |
| `-o <dir>` | 保存目录 / Output directory | 当前目录 / current dir |
| `-workers <n>` | 同时下载的文件数 / Files in parallel | 4 |
| `-conns <n>` | 最大同时连接数 / Max concurrent connections | 10 |
| `-segments <n>` | 大文件的分段数 / Segments per large file | 6 |
| `-seg-threshold <n>` | 超过多少字节用分段下载 / Segmented-download threshold (bytes) | 32 MiB |
| `-only <text>` | 只下载路径包含该文字的文件 / Only paths containing the text | — |
| `-attempts <n>` | 每段/每文件重试次数 / Retries per segment/file | 4 |
| `-stall <sec>` | 无数据多少秒判定停滞重试 / Stall timeout | 45 |
| `-proxy <url>` | 代理，如 `http://127.0.0.1:7890`、`socks5://127.0.0.1:1080` / Proxy URL | 自动读系统代理 / system proxy |
| `-quota-pause <sec>` | 509 限流时暂停的秒数 / Pause length on HTTP 509 | 300 |

例子 / Examples:

```bat
:: 只看清单 List only
MegaFolderDownloader.exe "链接/URL" -list

:: 全速下载 Full speed
MegaFolderDownloader.exe "链接/URL" -o D:\Courses -conns 16 -segments 8

:: 只下某个子目录 One subfolder only
MegaFolderDownloader.exe "链接/URL" -only "4. Core"
```

## 代理说明 / About proxies

浏览器能打开 MEGA、工具却连接失败，多半是工具没走代理。启动时工具按 **-proxy 参数 → 环境变量 → Windows 系统代理** 的顺序自动查找；Clash、V2Ray 等软件只要开着「系统代理」模式就会被自动读到。启动第一行会打印当前是否使用代理。TUN 模式下流量本来就走代理，直连即可。

If your browser opens MEGA but the tool cannot connect, the tool is probably not using your proxy. On startup it looks for a proxy in this order: **-proxy flag → environment variables → Windows system proxy**. Tools such as Clash or V2Ray are picked up automatically when their "system proxy" mode is on. The first line of output tells you whether a proxy is in use. With TUN mode, traffic is proxied anyway and no setting is needed.

## 限流 509 / Bandwidth limit (HTTP 509)

509 是 MEGA 按**出口 IP** 计算的免费流量配额用完了，不是工具故障；共享代理节点的配额可能被同节点的其他人用掉。最快解法是换一个代理节点（换出口 IP = 新的配额池）后重跑，已下内容会自动续传；或者不理会，工具会自动暂停并在配额恢复后继续。

HTTP 509 means the free transfer quota for your **egress IP** is exhausted — not a tool bug. On shared proxy nodes the quota may have been consumed by other users of the same node. The quickest fix is switching to another proxy node (a new egress IP has a fresh quota) and re-running; progress resumes automatically. Or just leave it running — the tool pauses and continues when the quota refills.

## 断点续传 / Resume

- 已下完的文件直接跳过；大文件下完的分段跳过，未下完的分段从断点字节处继续；小文件未完成的 `.part` 同样续传。
- 前提：不要手动删除下载目录中的 `.seg*` / `.part` 临时文件，它们保存着进度。

- Completed files are skipped; finished segments are skipped; partially downloaded segments and `.part` files resume from the exact byte offset.
- Just don't delete the `.seg*` / `.part` temp files in the download folder — they hold the progress.

## 从源码构建 / Build from source

需要 Go 1.23+。Requires Go 1.23+.

```bat
:: Windows exe
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -ldflags "-s -w" -o MegaFolderDownloader.exe .

:: Linux（测试用）/ Linux (for testing)
go build -o megadl .
```

## 说明 / Notes

- 仅支持公开文件夹链接（链接中带 `#` 密钥），不需要 MEGA 账号。
  Public folder links only (the ones with a `#` key); no MEGA account needed.
- 请只下载你有权获取的内容。
  Please only download content you are entitled to access.
