//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// restrictTokenFile 仅在 Windows 下生效：使用 icacls 把 token 文件的 DACL
// 设为仅当前用户可读写。os.WriteFile 的 0o600 在 Windows 上会被忽略，
// 文件会继承父目录 ACL，这里通过 icacls 显式锁定为当前用户独占。
func restrictTokenFile(path string) error {
	username := os.Getenv("USERNAME")
	if username == "" {
		return fmt.Errorf("cannot determine current Windows user")
	}
	// /grant <user>:(F) = 完全控制；/inheritance:r = 移除继承，只保留显式权限
	cmd := exec.Command("icacls", path, "/grant", username+":(F)", "/inheritance:r")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("icacls failed: %v (output: %s)", err, string(out))
	}
	return nil
}
