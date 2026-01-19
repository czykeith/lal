# Windows bcryptprimitives.dll 缺失问题解决方案

## 问题描述
运行 `lalserver.exe` 时出现错误：
```
fatal error: bcryptprimitives.dll not found
runtime: panic before malloc heap initialized
```

## 问题原因
`bcryptprimitives.dll` 是 Windows 系统的加密 API 库，Go 程序在使用 TLS/HTTPS 或其他加密功能时会依赖它。此错误通常由以下原因引起：
1. Windows 系统文件损坏或缺失
2. 系统更新不完整
3. 防病毒软件阻止了 DLL 加载
4. Windows 版本问题（精简版或修改版）

## 解决方案

### 方案 1：运行系统文件检查器（推荐）
以管理员身份运行命令提示符，执行：
```cmd
sfc /scannow
```
这会扫描并修复系统文件。

### 方案 2：运行 DISM 工具
以管理员身份运行 PowerShell，执行：
```powershell
DISM /Online /Cleanup-Image /RestoreHealth
```
然后再次运行：
```cmd
sfc /scannow
```

### 方案 3：检查 Windows 更新
确保 Windows 系统已更新到最新版本：
1. 打开"设置" > "更新和安全" > "Windows 更新"
2. 检查并安装所有可用更新
3. 重启计算机

### 方案 4：检查防病毒软件
1. 临时禁用防病毒软件
2. 尝试运行程序
3. 如果成功，将程序添加到防病毒软件的白名单

### 方案 5：重新编译程序（使用静态链接）
如果上述方法都不行，可以尝试重新编译程序，使用静态链接避免依赖系统 DLL：

```bash
# 设置环境变量，禁用 CGO（如果可能）
set CGO_ENABLED=0

# 重新编译
go build -ldflags="-s -w" -o lalserver.exe ./app/lalserver
```

注意：禁用 CGO 可能会影响某些功能，如果程序必须使用 CGO，此方法可能不适用。

### 方案 6：检查系统要求
确保系统满足以下要求：
- Windows 7 SP1 或更高版本
- 已安装 Visual C++ Redistributable（如果程序依赖）
- 系统架构匹配（32位/64位）

### 方案 7：手动复制 DLL（不推荐）
如果确定 DLL 文件存在但路径问题，可以：
1. 从其他正常工作的 Windows 系统复制 `bcryptprimitives.dll`
2. 放置到 `C:\Windows\System32\`（64位系统）或 `C:\Windows\SysWOW64\`（32位程序在64位系统）
3. 以管理员身份运行命令注册 DLL：
```cmd
regsvr32 bcryptprimitives.dll
```

## 验证修复
修复后，重新运行程序：
```cmd
D:\Stream\lal\lalserver.exe
```

如果问题仍然存在，请检查：
1. 程序日志文件（通常在程序目录下的 `logs` 文件夹）
2. Windows 事件查看器中的错误信息
3. 确认程序是否有其他依赖项缺失

## 预防措施
1. 定期更新 Windows 系统
2. 使用正版 Windows 系统
3. 避免使用精简版或修改版 Windows
4. 保持防病毒软件更新，但确保不会误报

## 相关链接
- [Microsoft Support: System File Checker](https://support.microsoft.com/en-us/topic/use-the-system-file-checker-tool-to-repair-missing-or-corrupted-system-files-79aa86cb-ca52-166a-92a3-966e85d4094e)
- [Go Issue: Windows DLL loading](https://github.com/golang/go/issues)
