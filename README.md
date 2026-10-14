# QQ 闪传下载器

这是一个用于公开 QQ 闪传分享链接（`https://qfile.qq.com/q/...`）的 Go 命令行程序。直接启动程序后，粘贴链接并按提示操作即可。   
亮点:1保留文件夹目录2自动解压缩3校验数据
## 交互式下载

编译后直接运行，或在项目目录执行：

```powershell
go run .
```

程序流程如下：

1. 粘贴 QQ 闪传短链接并按 Enter，程序解析全部文件和目录。
2. 再按 Enter，程序按顺序下载每一个可下载的文件。
3. 下载完成后再按 Enter，程序只检查**本次下载的文件**中的 `tar`、`gz`、`xz`、`zstd` 压缩包和分卷，例如 `a.tar` 与 `a.tar.002`，或 `a.tar.001`、`a.tar.002`。
4. 单文件压缩包会直接解压；分卷会按编号顺序流式合并后解压。原始压缩包和分卷会保留。
5. 程序会展示本次下载文件及本次解压文件的 MD5、SHA-1、SHA-256。
6. 哈希展示完成后，按 Enter 退出。

文件会保存到程序的当前运行目录，并保持分享页的目录层级。文件名来自 QQ 分享页的元数据，而不是 HTTP 的 `Content-Disposition` 响应头，因此不会出现 `wget` 那类响应头解析导致的乱码或错名。若同名文件已存在，会保存为 `文件名 (1).扩展名`，不会覆盖原文件。

支持单文件和编号分卷（`.NNN`）格式：

- `*.tar`
- `*.tar.gz` / `*.tgz`
- `*.tar.xz` / `*.txz`
- `*.tar.zst` / `*.tar.zstd` / `*.tzst`
- `*.gz`、`*.xz`
- `*.zst`、`*.zstd`

解压时会拒绝路径穿越以及符号链接等特殊归档项，避免压缩包写出当前目录。

哈希计算采用流式读取，不会一次性把大文件载入内存；MD5 与 SHA-1 仅用于兼容性校验，SHA-256 更适合完整性核验。

## 编译

```powershell
go build -o qfile-link.exe .
```

首次构建会下载纯 Go 的 `xz` 和 `zstd` 解压依赖。

## 保留的直链提取模式

给程序传入链接时，仍会输出 JSON 直链信息，方便脚本使用：

```powershell
go run . https://qfile.qq.com/q/TIRQPZgPUQ

# 仅输出“文件路径<TAB>直链”
go run . -text https://qfile.qq.com/q/TIRQPZgPUQ

# 保存 JSON 输出
go run . -output links.json https://qfile.qq.com/q/TIRQPZgPUQ
```

QQ 可能随时调整网页协议；若接口发生变更，需要相应更新程序中的接口常量和请求格式。
