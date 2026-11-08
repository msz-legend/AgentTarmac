// AgentTarmac 原生桌面壳（决策 D）：Electron 主进程。
// 职责：
//   1. 确保 daemon 在 127.0.0.1:8799 运行（不在则用与 launcher/start.ps1 完全一致的参数拉起）；
//   2. 若由本壳拉起 daemon，则一并拉起 opencode/hermes 常驻 connector（四端会话可用）；
//   3. 打开原生窗口加载控制台（Token 由服务端 BOOT 注入，localStorage 作兜底）。
"use strict";

const { app, BrowserWindow, shell, dialog } = require("electron");
const { spawn } = require("child_process");
const fs = require("fs");
const http = require("http");
const path = require("path");

const DAEMON_URL = (process.env.AGENTHUB_DAEMON_URL || "http://127.0.0.1:8799").replace(/\/+$/, "");
const ADDR = DAEMON_URL.replace(/^https?:\/\//, "");
const TOKEN_KEY = "agenthub_token";
const TITLE = "AgentTarmac";

// 本机规范数据目录：与 launcher/start.ps1、CC/Codex hook 共用同一份 token。
// 壳若另起 daemon 时复用此目录，token 与 hook 硬编码值一致，避免 401 → fail-closed 全 deny。
const SHARED_DATA_DIR = path.join("D:", "DevTools", "AgentHub", "data");

function resolveDataDir() {
  if (process.env.AGENTHUB_DATA) return process.env.AGENTHUB_DATA;
  try {
    if (fs.existsSync(path.join(SHARED_DATA_DIR, "token"))) return SHARED_DATA_DIR;
  } catch (_) {}
  return path.join(app.getPath("userData"), "data");
}

function tokenFileOf(dataDir) {
  return path.join(dataDir, "token");
}

function readToken(dataDir) {
  const candidates = [
    process.env.AGENTHUB_DATA && tokenFileOf(process.env.AGENTHUB_DATA),
    tokenFileOf(dataDir),
    tokenFileOf(SHARED_DATA_DIR),
  ].filter(Boolean);
  for (const p of candidates) {
    try {
      const raw = fs.readFileSync(p, "utf8").trim();
      if (raw.length >= 32) return raw;
    } catch (_) {}
  }
  return null;
}

// 可执行文件查找：环境变量 → 仓库构建产物 → 桌面包内 resources。
function findBinary(envKey, names) {
  const candidates = [];
  if (process.env[envKey]) candidates.push(process.env[envKey]);
  for (const n of names) {
    candidates.push(path.join(__dirname, "..", "bin", n)); // 开发态：desktop/../bin
    candidates.push(path.join(__dirname, "bin", n));
    candidates.push(path.join(process.resourcesPath || "", "bin", n)); // 打包态：resources/bin
    candidates.push(path.join(process.resourcesPath || "", n));
    candidates.push(path.join(__dirname, "..", n)); // 兼容旧布局
  }
  for (const p of candidates) {
    try {
      if (p && fs.existsSync(p)) return p;
    } catch (_) {}
  }
  return null;
}

function findIcon() {
  const candidates = [
    path.join(__dirname, "icon.ico"),
    path.join(process.resourcesPath || "", "icon.ico"),
    path.join(__dirname, "..", "launcher", "AgentTarmac.ico"),
  ];
  for (const p of candidates) {
    try {
      if (fs.existsSync(p)) return p;
    } catch (_) {}
  }
  return undefined;
}

function fetchHealth(timeoutMs) {
  return new Promise((resolve) => {
    const req = http.get(DAEMON_URL + "/healthz", { timeout: timeoutMs }, (res) => {
      res.resume();
      resolve(res.statusCode === 200);
    });
    req.on("timeout", () => {
      req.destroy();
      resolve(false);
    });
    req.on("error", () => resolve(false));
  });
}

let daemonProc = null;
const connectorProcs = [];

async function ensureDaemon(logger) {
  if (await fetchHealth(1500)) {
    logger("daemon already running at " + DAEMON_URL + "（复用，不重复拉起）");
    return { started: false, token: readToken(resolveDataDir()) };
  }

  const bin = findBinary("AGENTHUBD_PATH", ["agenthubd.exe", "agenthubd"]);
  if (!bin) {
    throw new Error(
      "未找到 agenthubd 可执行文件，也未检测到运行中的 daemon。\n" +
        "请先在仓库根目录执行 go build -o bin/agenthubd.exe ./cmd/agenthubd，\n" +
        "或用 AGENTHUBD_PATH 指定路径。"
    );
  }

  const dataDir = resolveDataDir();
  const hookBin = findBinary("HUB_HOOK_PATH", ["hub-hook.exe"]);
  const args = [
    "-addr",
    ADDR,
    "-data",
    dataDir,
    "-retention-days",
    "90",
    "-task-concurrency",
    "2",
  ];
  if (hookBin) args.push("-hook-path", hookBin);

  logger("spawning daemon: " + bin + " (data: " + dataDir + ")");
  daemonProc = spawn(bin, args, { stdio: ["ignore", "pipe", "pipe"], windowsHide: true });
  daemonProc.stdout.on("data", (d) => logger("[daemon] " + d.toString().trim()));
  daemonProc.stderr.on("data", (d) => logger("[daemon] " + d.toString().trim()));
  daemonProc.on("exit", (code) => logger("daemon exited: " + code));

  for (let i = 0; i < 40; i++) {
    if (await fetchHealth(500)) {
      return { started: true, token: readToken(dataDir) };
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error("daemon 启动超时（" + DAEMON_URL + "）");
}

// 仅在「本壳自己拉起了 daemon」时才拉 connector：
// 若 daemon 已在运行，说明 launcher/start.ps1 已把 connector 起好，避免重复进程。
async function ensureConnectors(logger, token) {
  const connectBin = findBinary("AGETHUB_CONNECT_PATH", ["agenthub-connect.exe"]);
  if (!connectBin) {
    logger("connector 可执行文件未找到，跳过 opencode/hermes 常驻接入");
    return;
  }
  const specs = [
    {
      kind: "opencode",
      envKey: "AGENTHUB_OPENCODE_PATH",
      candidates: [
        "D:\\DevTools\\npm-global\\node_modules\\opencode-ai\\bin\\opencode.exe",
      ],
      args: ["acp"],
      msys: false,
    },
    {
      kind: "hermes",
      envKey: "AGENTHUB_HERMES_PATH",
      candidates: [path.join(process.env.LOCALAPPDATA || "", "hermes", "bin", "hermes-acp.exe")],
      args: [],
      msys: true,
    },
  ];

  for (const s of specs) {
    const exe = process.env[s.envKey] || s.candidates.find((p) => p && fs.existsSync(p));
    if (!exe) {
      logger(s.kind + ": CLI 未安装，跳过（可用 " + s.envKey + " 指定）");
      continue;
    }
    const args = [
      "-daemon",
      DAEMON_URL,
      "-token",
      token || "",
      "-agent-kind",
      s.kind,
      "--",
      exe,
      ...s.args,
    ];
    logger("spawning connector: " + s.kind + " → " + exe);
    const env = Object.assign({}, process.env);
    if (s.msys) env.MSYS_NO_PATHCONV = "1";
    const p = spawn(connectBin, args, { stdio: ["ignore", "pipe", "pipe"], windowsHide: true, env });
    p.stdout.on("data", (d) => logger("[" + s.kind + "] " + d.toString().trim()));
    p.stderr.on("data", (d) => logger("[" + s.kind + "] " + d.toString().trim()));
    p.on("exit", (code) => logger(s.kind + " connector exited: " + code));
    connectorProcs.push(p);
  }
}

function createWindow(token) {
  const win = new BrowserWindow({
    width: 1440,
    height: 900,
    backgroundColor: "#0f1115",
    title: TITLE,
    icon: findIcon(),
    webPreferences: { contextIsolation: true, nodeIntegration: false },
  });
  win.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url); // 外链走系统浏览器
    return { action: "deny" };
  });
  win.loadURL(DAEMON_URL + "/").then(() => {
    // 服务端已把 token 注入 window.__TARMAC_BOOT__，这里只作兜底。
    if (token) {
      win.webContents
        .executeJavaScript(
          `localStorage.setItem(${JSON.stringify(TOKEN_KEY)}, ${JSON.stringify(token)}); "ok"`
        )
        .catch(() => {});
      win.webContents.reload();
    }
  });
  return win;
}

function cleanup() {
  for (const p of connectorProcs) {
    try {
      p.kill();
    } catch (_) {}
  }
  if (daemonProc) {
    try {
      daemonProc.kill();
    } catch (_) {}
  }
}

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", () => {
    const win = BrowserWindow.getAllWindows()[0];
    if (win) {
      if (win.isMinimized()) win.restore();
      win.focus();
    }
  });

  app.whenReady().then(async () => {
    const logger = (m) => console.log("[" + TITLE + "] " + m);
    try {
      const { started, token } = await ensureDaemon(logger);
      if (started) await ensureConnectors(logger, token);
      createWindow(token);
    } catch (err) {
      dialog.showErrorBox(TITLE + " 启动失败", String((err && err.message) || err));
      app.quit();
    }
  });

  app.on("window-all-closed", () => {
    cleanup();
    app.quit();
  });
}
