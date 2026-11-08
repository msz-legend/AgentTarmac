# computer-use 真人操作 helper：OS 级鼠标点击 + 键盘输入 + 窗口聚焦 + 截屏
# 用法： . 'D:\DevTools\AgentHub\qa\cu-helpers.ps1'
$ErrorActionPreference = 'Continue'
Add-Type -AssemblyName System.Windows.Forms, System.Drawing
Add-Type @'
using System;
using System.Runtime.InteropServices;
public class CU {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool BringWindowToTop(IntPtr h);
  [DllImport("user32.dll")] public static extern void SwitchToThisWindow(IntPtr h, bool alt);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int n);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, uint d, IntPtr e);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, IntPtr extra);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern int GetWindowText(IntPtr h, System.Text.StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern bool GetClientRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool ClientToScreen(IntPtr h, ref POINT p);
  [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
  public static void Down(){ mouse_event(0x0002,0,0,0,IntPtr.Zero); }
  public static void Up(){ mouse_event(0x0004,0,0,0,IntPtr.Zero); }
  public static void Wheel(int delta){ mouse_event(0x0800,0,0,(uint)delta,IntPtr.Zero); }
  public static void KDown(byte vk){ keybd_event(vk,0,0,IntPtr.Zero); }
  public static void KUp(byte vk){ keybd_event(vk,0,2,IntPtr.Zero); }
}
'@
$script:CUWindow = $null

function Get-CUWindow {
  $p = Get-Process AgentTarmac -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 } | Select-Object -First 1
  if (-not $p) { throw 'AgentTarmac 窗口未找到' }
  $script:CUWindow = $p
  return $p
}

function Focus-CU {
  $p = Get-CUWindow
  $h = $p.MainWindowHandle
  try { (New-Object -ComObject WScript.Shell).AppActivate($p.Id) | Out-Null } catch {}
  [void][CU]::ShowWindow($h, 3)
  [void][CU]::BringWindowToTop($h)
  [void][CU]::SwitchToThisWindow($h, $true)
  Start-Sleep -Milliseconds 450
  $fg = [CU]::GetForegroundWindow()
  return "focus hwnd=$h fg=$fg match=$($fg -eq $h)"
}

function Click-CU {
  param([int]$X, [int]$Y, [int]$Wait = 550)
  [void][CU]::SetCursorPos($X, $Y)
  Start-Sleep -Milliseconds 130
  [CU]::Down(); Start-Sleep -Milliseconds 60; [CU]::Up()
  Start-Sleep -Milliseconds $Wait
  return "clicked ($X,$Y)"
}

function DoubleClick-CU {
  param([int]$X, [int]$Y, [int]$Wait = 550)
  [void][CU]::SetCursorPos($X, $Y); Start-Sleep -Milliseconds 100
  [CU]::Down(); [CU]::Up(); Start-Sleep -Milliseconds 90
  [CU]::Down(); [CU]::Up()
  Start-Sleep -Milliseconds $Wait
}

function Key-CU {
  param([string]$Key = 'Enter', [int]$Wait = 350)
  # 用 SendKeys 表达功能键；普通字符也支持
  [System.Windows.Forms.SendKeys]::SendWait($Key)
  Start-Sleep -Milliseconds $Wait
}

function Type-CU {
  param([string]$Text, [int]$Wait = 400)
  # 中文/任意文本：剪贴板 + Ctrl+V（真人常用输入方式），避免 SendKeys 对中文失效
  Set-Clipboard -Value $Text
  Start-Sleep -Milliseconds 120
  [CU]::KDown(0x11); [CU]::KDown(0x56)
  Start-Sleep -Milliseconds 60
  [CU]::KUp(0x56); [CU]::KUp(0x11)
  Start-Sleep -Milliseconds $Wait
  return "typed: $Text"
}

function Scroll-CU {
  param([int]$Delta = -360, [int]$Wait = 350)
  [CU]::Wheel($Delta); Start-Sleep -Milliseconds $Wait
}

function Shot-CU {
  param([string]$Name = 'shot')
  $p = Get-CUWindow
  $r = New-Object CU+RECT
  [void][CU]::GetWindowRect($p.MainWindowHandle, [ref]$r)
  $x = [Math]::Max(0, $r.Left); $y = [Math]::Max(0, $r.Top)
  $w = [Math]::Min(1920 - $x, $r.Right - $x); $ht = [Math]::Min(1944 - $y, $r.Bottom - $y)
  $bmp = New-Object System.Drawing.Bitmap($w, $ht)
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen($x, $y, 0, 0, $bmp.Size)
  $out = "D:\DevTools\AgentHub\qa\$Name.png"
  $bmp.Save($out, [System.Drawing.Imaging.ImageFormat]::Png)
  $g.Dispose(); $bmp.Dispose()
  return "shot=$out rect=($x,$y) ${w}x${ht}"
}
