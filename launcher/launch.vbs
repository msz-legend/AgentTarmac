' AgentTarmac desktop launcher — run PowerShell hidden, no console flash.
' Double-clicking the desktop shortcut invokes this file via wscript.exe.
Option Explicit
Dim sh, cmd
Set sh = CreateObject("WScript.Shell")
cmd = "powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File ""D:\DevTools\AgentHub\launcher\start.ps1"""
sh.Run cmd, 0, False
