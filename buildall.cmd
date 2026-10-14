@echo off
setlocal enabledelayedexpansion

rem 创建输出目录
if not exist "dist" mkdir "dist"

rem 定义平台列表（格式: os/arch）
set "platforms=linux/amd64 linux/arm64 windows/amd64 windows/386 darwin/amd64 darwin/arm64"

echo 全平台交叉编译开始...
echo.

for %%p in (%platforms%) do (
    rem 正确分割 "os/arch" 为两部分
    for /f "tokens=1,2 delims=/" %%a in ("%%p") do (
        set "GOOS=%%a"
        set "GOARCH=%%b"

        rem 构建输出文件名
        set "outfile=dist\app-!GOOS!-!GOARCH!"
        if "!GOOS!"=="windows" set "outfile=!outfile!.exe"

        echo 📦 !GOOS! / !GOARCH! -^> !outfile!

        rem 关键：在同一行设置所有环境变量 + 执行编译（避免环境变量污染）
        set "GOOS=!GOOS!" & set "GOARCH=!GOARCH!" & set "CGO_ENABLED=0" & go build -ldflags="-s -w" -o "!outfile!" .

        if errorlevel 1 (
            echo ❌ 构建失败: !GOOS!/!GOARCH!
            exit /b 1
        )
    )
)

echo.
echo ✅ 构建成功! 二进制文件位于: dist\
endlocal