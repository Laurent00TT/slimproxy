# slimproxy CLI 改造实施计划（历史文档）

> **这是 2026-07 改造期间的实施计划，已全部完成，按原样保留作为决策记录。**
> 文中的「待做」「阶段」描述的是当时的计划状态，不是现在的路线图；
> 当前的真实形态以 [ARCHITECTURE.md](ARCHITECTURE.md) 与 README 为准。

从「HTTP 服务 + Web 仪表盘」改造为「CLI 工具」，形态对标 Claude Code CLI。

## 已确定的架构决策

| 决策 | 选择 | 后果 |
|---|---|---|
| 进程模型 | 同进程，TUI 退出 = 服务停止 | 状态零延迟；不做 IPC；关终端即停 |
| 隧道管理 | slimproxy spawn/kill cloudflared | `tunnel up/down` 是真开关；要管子进程生命周期 |
| UI 代码 | 彻底删除，含四个配置项 | 「反代后 peer 判断失效」整类问题消失 |
| TUI 技术 | bubbletea + lipgloss | 引入 Charm 依赖 |
| 子命令解析 | 标准库手写，不引入 cobra | 零依赖，符合 slim 调性 |

## 贯穿全程的约束

1. **每个节点结束时 `go build` / `go vet` / `go test` 必须全绿**，且旧行为不回归。
2. **不引入静默失效。** 这个项目已经踩过三次：旧二进制忽略新配置字段、
   `SetPluginHooks` 被启动流程擦除、`request-log-dir` 配了不生效。任何"配了没生效"
   都必须变成可见的错误或告警。
3. **动作层与展示层分离。** TUI 的斜杠命令和 CLI 子命令必须调用同一个动作函数，
   不允许两套实现。这是阶段四能低成本落地的前提。
4. **破坏性操作前备份。** 项目非 git 仓库（已建议 `git init`）。

---

## 阶段一 · 移除 Web UI ✅ 已完成

**产出**：`metrics` 包；`proxy.Runtime`；删除 `ui/` 与四个配置项。

**验证**：build/vet/test 全绿；`/ui` 与 `/ui/events` → 404；API 与隧道功能不变。

---

## 阶段二 · CLI 骨架

**目标**：把单一 `flag.Parse()` 换成子命令路由，且不破坏任何现有用法。

### 任务

1. 新建 `cmd/slimproxy/cli.go`：命令注册表 + 分发器。
   ```go
   type command struct {
       name    string
       summary string          // help 里的一行说明
       usage   string          // 详细用法
       run     func(ctx *cliContext, args []string) error
   }
   ```
2. `cliContext` 承载跨命令的公共状态（configPath、stateDir、已加载的 Config）。
   延迟加载配置：`init` 和 `routes` 不需要配置文件存在。
3. 实现分发：`os.Args[1]` 匹配命令名；无参数时进入默认行为（阶段四前 = 启动服务）。
4. 现有 flag 保留为别名：`-check` → `check`，`-init` → `init`，`-routes` → `routes`。
   **必须保留**，否则任何现存脚本和文档全部失效。
5. `slimproxy help` / `-h` 输出命令列表。

### 验证标准

- 旧用法逐条实测：`-check` / `-init` / `-routes` / `-port` / `-config` / `-state` / `-force`
- 新用法：`check` / `init` / `routes` / `help`
- 无参数启动服务，行为与改造前一致
- 未知命令给出可用命令列表，退出码非 0

### Review 节点 A
- **agent**：`pr-review-toolkit:code-reviewer`
- **重点**：命令分发的边界情况（空参数、`--` 之后的参数、flag 与子命令混用）；
  向后兼容是否真的完整；错误信息是否可操作。

---

## 阶段三 · 命令实现

拆成三个独立节点，按风险从低到高排列，每个节点独立可交付、独立 review。

### 3a · `status` 与 `doctor`（只读，风险最低）

**`status`**：一次性快照。listen 地址、uptime、rpm/TTFT、凭据数与冷却状态、
隧道三态、配额占用。

**`doctor`**：把这个项目踩过的坑做成自动检测，每项输出 PASS/WARN/FAIL + 修复建议。

| 检查项 | 判据 | 来源 |
|---|---|---|
| 端口占用 | 绑定 8317 前先探测 | 实际撞过，exit 1 被日志淹没 |
| fake-ip 劫持 | 解析 `api.anthropic.com` / `region1.v2.argotunnel.com`，命中 `198.18.0.0/15` 即告警 | 隧道和上游都中过招 |
| 隧道连接数 | `cloudflared tunnel info` 连接数为 0 但进程在跑 | 「看起来在跑其实每几分钟断一次」 |
| 凭据状态 | auth-dir 文件可解析、未过期、不在冷却 | — |
| 配置与二进制匹配 | 配置中存在但本二进制不认识的字段 | 旧 exe + 新字段 = 静默失效 |
| 上游可达性 | 到 `api.anthropic.com` 的 TCP + TLS | 偶发 500/21s 超时 |

**验证**：制造每一种故障并确认 doctor 报出来（能安全模拟的：占端口、断隧道、
配置加未知字段）。不能只测 happy path。

**Review 节点 B**
- **agent**：`pr-review-toolkit:silent-failure-hunter`
- **重点**：doctor 自身的失败处理——检测项超时、外部命令不存在、权限不足时，
  必须报「无法检测」而不是 PASS。一个会把未知当健康的诊断工具比没有更糟。

### 3b · `tunnel`（子进程管理）

`tunnel setup`：login → create → route dns → 生成 config.yml（复用
`deploy/install-tunnel-config.ps1` 的校验逻辑，改为 Go 实现，跨平台）。
`tunnel up` / `down`：spawn/kill cloudflared，日志转发到 slimproxy 日志。
`tunnel status`：三态 —— 未配置 / 已配置未运行 / 运行中（含连接数与边缘节点）。

**关键实现点**
- slimproxy 退出时必须杀掉 cloudflared 子进程，否则留下孤儿进程占着隧道。
  Windows 上没有进程组信号，需显式 kill。
- `protocol: http2` 的取舍要保留在生成的配置注释里（那是环境特定的权宜之计，
  不是推荐值）。
- 记录 fake-ip 判断：连接的边缘 IP 若在 `198.18.0.0/15`，`status` 要标注
  「经由本地代理，长连接可能不稳」。

**验证**：up → status 显示运行中且连接数 > 0 → 外网 curl 200 → down → 外网 curl 失败
→ 确认无残留 cloudflared 进程。

**Review 节点 C**
- **agent**：`pr-review-toolkit:code-reviewer`
- **重点**：子进程生命周期——异常退出、slimproxy 崩溃、Ctrl-C、重复 up 的处理；
  孤儿进程；cloudflared 不存在或版本不兼容时的报错质量。

### 3c · `auth`（OAuth 流程，最重）

`auth list`：凭据表格（provider、账号、状态、冷却、模型数）。
`auth add <provider>`：在终端完成 OAuth。xAI/Kimi 是设备码（终端显示即可）；
Claude/Codex/Antigravity 需要浏览器回调。
`auth rm <id>`：删除凭据文件（需确认）。
`auth refresh`：手动触发刷新。

**关键实现点**
- OAuth 回调原先由 dashboard 的 HTTP 路由接收。CLI 需要另起临时本地监听，
  或使用 provider 支持的手工粘贴 code 方式。**先调研 CLIProxyAPI 的
  `RequestAnthropicToken` 等返回什么，再决定**——它返回 `{"status":"ok","url":...,"state":...}`，
  说明回调仍需一个 HTTP 端点。
- `auth rm` 是删除操作，必须二次确认，且默认不删最后一个可用凭据。

**验证**：完整跑一次 add（至少一个 provider）→ list 能看到 → 实际发请求能用 → rm 后消失。

**Review 节点 D**
- **agent**：`pr-review-toolkit:code-reviewer` + `pr-review-toolkit:silent-failure-hunter`
- **重点**：OAuth 临时监听的安全性（绑定地址、超时、state 校验）；
  凭据文件权限（0600）；删除操作的确认与可恢复性。

---

## 阶段四 · TUI 外壳

**前提**：阶段三的动作层稳定，TUI 只做渲染与输入分发。

1. 布局方案：先给 2–3 个候选（ASCII 草图 + 实际渲染截图）让用户选，不直接写完。
2. bubbletea Model/Update/View；状态源为 `proxy.Runtime.Stats` 的定时 `Snapshot`。
3. 斜杠命令复用阶段三动作函数。
4. 降级：非 TTY 环境（管道、CI）自动退回 `serve` 行为，不试图渲染。

**Review 节点 E**
- **agent**：`code-simplifier:code-simplifier`
- **重点**：TUI 与动作层是否真的解耦；有无为了渲染方便而在动作层塞展示逻辑。

---

## 节点检查清单（每个节点结束时逐条确认）

- [ ] `go build ./...` / `go vet ./...` / `go test ./...` 全绿
- [ ] 新增行为有测试覆盖，且测试能因真实 bug 失败（不是断言恒真）
- [ ] 旧行为实测未回归
- [ ] 无新增静默失效路径
- [ ] 派发 review agent，处理其发现
- [ ] 更新记忆文件的进度段
