@echo off
chcp 65001 >nul 2>nul
rem imgsim 一键编译脚本：机台图片模拟器（Go + lxn/walk）
rem 环境要求：Go 1.21 或更高版本并已加入 PATH；可直接双击运行 build.bat
cd /d "%~dp0"

where go >nul 2>nul
if errorlevel 1 goto nogo

if exist rsrc.syso goto build
where rsrc >nul 2>nul
if errorlevel 1 goto build
echo 正在根据 app.manifest 生成 rsrc.syso ...
rsrc -manifest app.manifest -arch amd64 -o rsrc.syso

:build
if not exist dist mkdir dist
echo 正在编译 dist\imgsim.exe ...
go build -trimpath -ldflags="-H windowsgui -s -w" -o dist\imgsim.exe .
if errorlevel 1 goto fail
echo.
echo 编译成功：dist\imgsim.exe
exit /b 0

:nogo
echo 未在 PATH 中找到 Go 工具链。
echo 请先安装 Go 1.21 或更高版本，然后重新运行本脚本。
exit /b 1

:fail
echo 编译失败，请根据上方错误信息排查。
exit /b 1
