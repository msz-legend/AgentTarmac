// AgentTarmac 桌面预加载脚本（PC 端原生能力桥）。
// contextIsolation 开启，nodeIntegration 关闭：只通过 contextBridge 暴露最小必要 API。
"use strict";

const { contextBridge, ipcRenderer } = require("electron");

// 原生目录选择：调用 Electron dialog.showOpenDialog({properties:["openDirectory"]})
contextBridge.exposeInMainWorld("tarmac", {
  pickDirectory: async () => {
    const { canceled, filePaths } = await ipcRenderer.invoke("tarmac:pick-directory");
    if (canceled) return null;
    return filePaths[0] || null;
  },
});
