# 贡献指南

感谢你对 AgentHub 的兴趣！AgentHub 是 Open Core 项目（核心 MIT 许可，商业插件 / 企业版另行授权）。

## 分支策略

- `main`：稳定版，仅合并经过 CI 验证的 PR。
- 功能分支：`feature/<描述>`，从 `main` 分出，合并后删除。
- 热修复：`hotfix/<描述>`，从最新 tag 分出。

## 提交规范

采用 Conventional Commits：

```
<type>(<scope>): <subject>

<scope> 可为：daemon, connector, pty, desktop, docs, ci, security
```

示例：
```
feat(pty): add ConPTY session + WebSocket terminal endpoint
fix(console): taskRowHTML parameter shadowing global i18n t()
docs(api): add terminal-ws endpoint reference
```

## 触发 CI

每次 `push` 到 `main` 或 PR 触发 GitHub Actions（见 `.github/workflows/ci.yml`）：
1. `go build ./...`
2. `go vet ./...`
3. `go test ./...`
4. `gofmt -l .`

## 发布流程

1. 在 `main` 上创建 release PR，更新 `CHANGELOG.md`。
2. 维护者 review 后合并，自动触发 GitHub Release（构建 NSIS 安装包 + 便携版）。
3. 打 tag `v<major>.<minor>.<patch>`。

## 行为准则

见 [`CODE_OF_CONDUCT.md`](code-of-conduct.md)。
