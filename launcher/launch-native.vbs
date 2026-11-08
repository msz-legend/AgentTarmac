' AgentTarmac 原生桌面壳启动入口（无控制台闪烁）。
' 关键防御：DSH/部分终端会向子进程注入 ELECTRON_RUN_AS_NODE=1，
' 使 Electron 二进制退化为纯 Node 并静默退出（exit 0，无窗口）。
' 桌面双击由 explorer.exe 启动时环境干净，但此包装保证任何父进程都可用。
Option Explicit
Dim sh, exe, env
Set sh = CreateObject("WScript.Shell")
On Error Resume Next
sh.Environment("PROCESS").Remove "ELECTRON_RUN_AS_NODE"
sh.Environment("PROCESS").Remove "ELECTRON_ENABLE_LOGGING"
On Error Goto 0
exe = "D:\DevTools\AgentHub\desktop\dist\win-unpacked\AgentTarmac.exe"
sh.CurrentDirectory = "D:\DevTools\AgentHub\desktop\dist\win-unpacked"
sh.Run """" & exe & """", 1, False
