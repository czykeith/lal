@echo off
REM Windows DLL 修复脚本
REM 用于修复 bcryptprimitives.dll 缺失问题

echo ========================================
echo Windows DLL 修复工具
echo ========================================
echo.

REM 检查管理员权限
net session >nul 2>&1
if %errorLevel% neq 0 (
    echo [错误] 请以管理员身份运行此脚本！
    echo 右键点击此文件，选择"以管理员身份运行"
    pause
    exit /b 1
)

echo [1/4] 正在运行系统文件检查器 (sfc /scannow)...
echo 这可能需要几分钟时间，请耐心等待...
echo.
sfc /scannow
if %errorLevel% equ 0 (
    echo [成功] 系统文件检查完成
) else (
    echo [警告] 系统文件检查遇到问题，继续下一步...
)
echo.

echo [2/4] 正在运行 DISM 工具修复系统映像...
echo 这可能需要更长时间，请耐心等待...
echo.
DISM /Online /Cleanup-Image /RestoreHealth
if %errorLevel% equ 0 (
    echo [成功] DISM 修复完成
) else (
    echo [警告] DISM 修复遇到问题
)
echo.

echo [3/4] 再次运行系统文件检查器...
sfc /scannow
echo.

echo [4/4] 检查 Windows 更新状态...
echo 建议手动检查 Windows 更新以确保系统完整
echo.

echo ========================================
echo 修复完成！
echo ========================================
echo.
echo 请执行以下步骤：
echo 1. 重启计算机（如果提示）
echo 2. 重新运行 lalserver.exe
echo 3. 如果问题仍然存在，请查看 WINDOWS_DLL_FIX.md 获取更多解决方案
echo.
pause
