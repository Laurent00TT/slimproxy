# slimproxy 使用指南

从零开始，不需要预备知识。照着做就能跑起来。

---

## 目录

1. [这个工具解决什么问题](#1-这个工具解决什么问题)
2. [准备](#2-准备)
3. [五分钟跑起来](#3-五分钟跑起来)
4. [确认它真的在工作](#4-确认它真的在工作)
5. [全屏面板](#5-全屏面板)
6. [命令详解](#6-命令详解)
7. [配置文件](#7-配置文件)
8. [暴露到公网](#8-暴露到公网)
9. [出问题了](#9-出问题了)
10. [安全须知](#10-安全须知)

---

## 1. 这个工具解决什么问题

假设你有 Claude 的订阅（月付那种，不是按 token 计费的 API）。

订阅只能通过官方客户端用。但你想：

- 用别的工具（比如某个只支持 OpenAI 接口的编辑器插件）连上 Claude
- 在另一台电脑上用
- 让几个朋友也能用

slimproxy 做的事：**在你本机开一个端口，假装成 OpenAI/Anthropic/Gemini 的 API，
背后用你的订阅凭据去请求真正的上游。**

> **先想清楚再用**：多数订阅的服务条款只允许在官方客户端里使用。把订阅凭据
> 通过任何代理暴露出去——哪怕只给自己——都可能违反条款，代价是账号被停。
> 给别人用之前更要想清楚：**你在把整个账号的额度和风险交出去**，只把访问
> 权给你愿意共享整个账号的人。本项目不为任何用法背书，条款自己读，决定自己做。

```
你的编辑器插件  ──OpenAI 协议──▶  slimproxy  ──订阅凭据──▶  Anthropic
（只会说 OpenAI 话）                （当翻译）              （听 Claude 话）
```

所以你可以让一个只认 OpenAI 接口的工具，实际上在用 Claude。

**它不做什么**：不帮你绕过任何限制，不改变你订阅的额度，不做负载均衡或缓存。
它就是一个协议翻译 + 凭据管理的中间层。

---

## 2. 准备

**需要：**

- Go 1.26 或更新（用来构建）—— [下载](https://go.dev/dl/)
- 一个 AI 服务的订阅账号（Claude / Codex / Gemini / Kimi / xAI 任一）
- 一个终端（Windows 上用 PowerShell 或 Git Bash 都行）

**构建：**

```bash
cd slimproxy
go build -o slimproxy.exe ./cmd/slimproxy
```

Linux/macOS 上去掉 `.exe`，下文的 `slimproxy.exe` 相应改成 `./slimproxy`。

第一次构建会下载依赖，可能要几分钟。

依赖全部来自公开的 Go module proxy，克隆下来即可构建，无需其他准备。

---

## 3. 五分钟跑起来

### 第一步：生成配置

```bash
slimproxy.exe init
```

它会：

- 创建 `slimproxy.yaml`，里面有一个**随机生成的 API key**
- 创建 `auths/` 目录（存放凭据）
- 打印接下来该做什么

输出长这样：

```
已写入 slimproxy.yaml

接下来:

  1. 添加一个上游凭据（会打开浏览器完成授权）
       slimproxy.exe auth add claude

     可用的 provider: antigravity / claude / codex / kimi / xai
     ...

  3. 把客户端指过来
       OpenAI 兼容      http://127.0.0.1:8317/v1
       ...
     Bearer <你机器上生成的随机 key，此处示例已替换>

```

**把那个 Bearer 后面的字符串记下来**，这是你的客户端要用的 API key。
忘了也没关系，它就在 `slimproxy.yaml` 里。

### 第二步：添加凭据

```bash
slimproxy.exe auth add claude
```

会打开浏览器让你登录。授权完成后凭据文件写进 `auths/`。

如果浏览器没自动打开，或者你想用另一个浏览器登录另一个账号：

```bash
slimproxy.exe auth add claude -no-browser
```

它会打印一个链接让你自己粘到浏览器里。

### 第三步：启动

```bash
slimproxy.exe
```

在终端里运行会打开一个全屏面板。看到面板就说明起来了。

按 `q` 退出（这也会停掉代理）。

### 第四步：连上它

在你的客户端里填：

| 客户端类型 | Base URL | API Key |
|---|---|---|
| OpenAI 兼容 | `http://127.0.0.1:8317/v1` | 第一步那个随机串 |
| Anthropic SDK | `http://127.0.0.1:8317` | 同上 |
| Gemini SDK | `http://127.0.0.1:8317` | 同上 |

**Claude Code 的例子**（在另一台电脑或另一个终端）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8317
export ANTHROPIC_API_KEY=你的随机串
claude
```

---

## 4. 确认它真的在工作

别只看面板上没有红字就以为好了。发一个真实请求：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer 你的随机串" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"say hi"}]}'
```

**成功**：返回一段 JSON，里面有回复内容。同时面板的「最近请求」里会多一行。

**返回 401**：API key 不对。检查 `slimproxy.yaml` 里的 `api-keys`。

**返回 502 或超时**：上游有问题。运行 `slimproxy.exe doctor`。

顺带一提，健康检查不需要 key：

```bash
curl http://127.0.0.1:8317/healthz
```

---

## 5. 全屏面板

在终端里运行 `slimproxy.exe`（不带参数）就会进面板。

```
┌─ slimproxy 0.1.0 ──────────────────────────────────────────┐
│ 监听 127.0.0.1:8317  运行 4h12m  rpm 17  ttft 842ms        │
├────────────────────────────────────────────────────────────┤
│ 隧道  proxy.example.com         ● 2 连接                   │
│ 凭据  claude · you@example.com  ● 7h24m 后过期             │
│ 诊断  PASS 4 · WARN 1           ▲ 3m12s前                  │
├─ 活跃路由 ─────────────────────────────────────────────────┤
│ openai → claude                        41    812ms     68% │
├─ 最近请求 ─────────────────────────────────────────────────┤
│ 12:41:07 ok  openai → claude  opus-5        812ms     1.9k │
│ 12:40:58 429 openai → claude  opus-5            —        — │
└────────────────────────────────────────────────────────────┘
 / 命令   r 刷新   q 退出
```

### 怎么读这些数字

| 位置 | 含义 |
|---|---|
| `rpm 17` | 最近**一分钟**内完成了 17 个请求 |
| `ttft 842ms` | 平均首字延迟（发出请求到收到第一个字），过去一小时 |
| `● / ▲ / ·` | 绿点=正常，红三角=需要注意，灰点=未知或未配置 |
| 活跃路由 | 过去一小时哪些协议翻译对有流量，以及各自的次数/平均延迟/占比 |
| 最近请求 | `ok` 表示成功；数字（如 `429`）是上游返回的真实状态码 |

**关于状态码**：成功显示 `ok` 而不是 `200`，因为这个进程实际上观测不到成功的状态码。
失败显示真实的码，因为 `401`（凭据失效，要重新登录）和 `429`（被限流，等一会）
需要完全不同的应对。

### 按键

| 键 | 作用 |
|---|---|
| `/` | 打开命令输入 |
| `r` | 立刻刷新隧道和凭据状态（不用等轮询） |
| `q` | 退出（同时停止代理） |
| `Esc` | 关闭输出面板 |
| `↑` `↓` | 滚动输出面板 |
| `Ctrl+C` | 强制退出，任何情况下都有效 |

### 斜杠命令

按 `/` 之后会列出可用命令，边打字边筛：

```
   tunnel status  查询隧道三态与 Cloudflare 侧连接数
   tunnel up      启动 cloudflared 并等待注册连接
   tunnel down    停止本进程启动的 cloudflared
     当前 2 连接 · 停止后 proxy.example.com 立即不可达
 › /tunnel d
```

注意**选中行下面那句话**——破坏性操作会在你按下 Enter 之前告诉你代价。
`tunnel down` 不是说「停止隧道」，而是说清楚现在有几个连接、停了之后哪个域名会失联。

可用命令：`tunnel status|up|down`、`auth list|rm`、`doctor`、`routes`、`quit`。

`auth add` 在面板里**不能用**——它需要浏览器交互。面板会告诉你退出后跑命令行版本。

---

## 6. 命令详解

### `slimproxy`（不带参数）

启动代理。在终端里会同时打开面板；输出被重定向时（比如 `slimproxy > log.txt`）
自动退回纯日志模式。

想在终端里也不要面板：

```bash
slimproxy.exe -no-tui
```

---

### `slimproxy check` — 启动前先看看

**什么时候用**：改完配置想确认没写错，但不想真的占端口。

```bash
slimproxy.exe check
```

输出：

```
配置 OK

  监听      127.0.0.1:8317
  凭据目录  C:\...\auths
  状态目录  .
  入站认证  需要 1 个 key
  应用日志  stdout（log-to-file 关闭）
  请求日志  关闭
  上游重试  request-retry=3 max-retry-interval=30s max-retry-credentials=0
  模型      已加载凭据暴露的全部模型
```

它**只读配置**，不连网络、不碰凭据、不绑端口。配置有错会指名道姓：

```
slimproxy: 读取配置 "slimproxy.yaml" 失败: yaml: unmarshal errors:
  line 8: field ui not found in type proxy.Config
```

> 未知的配置项是**错误**而不是被忽略。写错一个键名，程序拒绝启动并告诉你是哪个键——
> 而不是让你花一小时琢磨「为什么我的设置没生效」。

---

### `slimproxy status` — 现在什么情况

**什么时候用**：想知道代理有没有在跑、凭据还剩多久、隧道通不通。

```bash
slimproxy.exe status
```

**永远退出码 0**，所以适合放进脚本轮询。它只报告观察到的事实，不做判断。

和 `check` 的区别：`check` 看配置文件，`status` 看**当前实际状态**（端口、凭据、隧道）。

---

### `slimproxy doctor` — 出问题了跑这个

**什么时候用**：不工作，但不知道哪里坏了。

```bash
slimproxy.exe doctor
```

跑六项检查，每项给出结论和修复建议：

```
  PASS     listen-port      127.0.0.1:8317 已被 slimproxy 占用
  PASS     credentials      1 个凭据可解析（未校验有效期与冷却状态）
  PASS     config-fields    没有未知字段
  WARN     upstream-dns     api.anthropic.com 解析到 198.18.0.101（fake-ip 段）
           → 让该域名绕过本地代理，或在 hosts 中固定真实地址
  PASS     upstream-reach   TCP 可连接（未验证 TLS）
  PASS     tunnel           proxy.example.com，2 个连接

  1 WARN · 5 PASS，用时 6.2s
```

**有问题时退出码非 0**，适合放进 CI 或启动脚本。

四种结论：

| | 含义 |
|---|---|
| `PASS` | 查了，没问题 |
| `WARN` | 能用，但以后会咬你 |
| `FAIL` | 坏了 |
| `UNKNOWN` | **查不出结论**（超时、缺工具、没权限） |

`UNKNOWN` 的严重程度**高于 WARN**。理由是：一个持有你订阅凭据的进程上，
一个没查清楚的问题，比一个已知无害的告警更值得你花时间。

> **三个"检查"命令怎么选**：
> `check` 看配置写得对不对（不碰运行时）→
> `status` 看现在是什么状态（不做判断）→
> `doctor` 看哪里坏了该怎么修（会连网，慢）。

---

### `slimproxy auth` — 管凭据

```bash
slimproxy.exe auth list          # 看池子里有什么
slimproxy.exe auth add claude    # 加一个（开浏览器）
slimproxy.exe auth rm <名字>      # 删一个
```

**`auth list` 输出**：

```
  名称                   PROVIDER  账号              状态    到期
  claude-you.json        claude    you@example.com   可用    7h24m 后

  共 1 个，其中 1 个当前可用
```

状态有六种：`可用` / `即将过期` / `已过期` / `已禁用` / `损坏` / `未知`。

**「已过期」不等于坏了。** access token 过期但 refresh token 还在的话，
代理会自己续期。真正没救的只有 `损坏`（文件读不懂）和 `已禁用`。

**`auth add` 的参数**：

```bash
slimproxy.exe auth add claude -no-browser        # 打印链接，自己粘贴
slimproxy.exe auth add claude -callback-port N   # 换回调端口
```

**`auth rm` 的保护**：删除最后一个可加载的凭据会被拒绝——因为那样代理还会正常启动、
接受请求，然后每一个都失败，从外面看完全健康。真要删：

```bash
slimproxy.exe auth rm <名字> -force
```

---

### `slimproxy tunnel` — 公网通道

见[第 8 节](#8-暴露到公网)。

---

### `slimproxy routes` — 支持哪些翻译

```bash
slimproxy.exe routes
```

列出这个构建能翻译哪些「客户端协议 → 上游协议」的组合。

**什么时候看**：你的客户端说 A 协议，你的凭据是 B 家的，想确认这条路通不通。

---

### `slimproxy log` — 回溯发生过什么

**什么时候用**：「刚才那个请求为什么失败」「昨天下午为什么卡」「配额还剩多少」。

```bash
slimproxy.exe log                      # 最近 1 小时
slimproxy.exe log -since 24h -failed   # 一天内所有失败
slimproxy.exe log -slow 10s            # 慢于 10 秒的请求
slimproxy.exe log -status 429          # 被限流的
slimproxy.exe log -since 7d -stats     # 只看汇总
```

```
  时间              状态  路由             模型      首字    总时长  说明
  07-26 21:42:25  ▸ 隧道  up · proxy.example.com · 2 连接
  07-26 21:43:01  ok    openai → claude  opus-5   812ms   3.1s   claude-you · 配额 62%
  07-26 21:47:18  429   openai → claude  opus-5   —       20.0s  claude-you · 配额 98%

  最近 1 小时：2 个请求，1 个失败（429×1）
  时延 p50 3.1s，最慢 20.0s
  5 小时窗口用量峰值 98%
  另有 37 次外部被拒请求（扫描噪音，未计入上面的失败数）
```

**这张表最值钱的地方是把请求和状态变化放在同一条时间线上。** 上面那个 429
旁边就写着「配额 98%」——原因一目了然，不需要去别处对时间戳。隧道启停、凭据删除、
诊断结论也都在这条线上。

**关于「扫描噪音」**：你的隧道暴露在公网，会被持续扫描（探测 `/api/hello` 这类路径）。
这些请求默认不列出，但**会被计数**——因为把它们混进错误率会让那个数字失去意义，
而完全不记又回答不了「有多少东西在敲我的门」。要看明细加 `-noise`。

底层是 `logs/events/YYYY-MM-DD.jsonl`，一行一个 JSON。所以你随时可以自己用 jq 查
任何这个命令没提供的问法。默认保留 7 天，`journal-days` 可改。

**不含请求和响应的正文**——那是 `request-log` 的职责，而且危险。这份日志设计成
可以放心保留一周、随手翻看的东西。

---

### `slimproxy test` — 到底能不能用

**什么时候用**：`doctor` 全绿，但你想确认真的能发出一个请求并拿到回复。

```bash
slimproxy.exe test
```

它向本机代理发一个最小的请求，走完整条链路：入站认证 → 协议翻译 → 上游凭据 → 真实响应。

```
目标 http://127.0.0.1:8317
模型 claude-opus-5（未指定，取自 /v1/models 的第一个）
这会向上游发出一个真实请求，消耗订阅配额。

成功，耗时 1.2s
  finish_reason  stop
  tokens         14
  回复           OK
```

**注意它会消耗你的订阅额度**（一次约十几个 token）。所以它是一条要手敲的命令，
而不是 `doctor` 的一部分——诊断命令可能被放进定时任务，不该花钱。

`doctor` 检查的是端口、凭据文件能否解析、DNS、上游 TCP 可达——这些都停在
翻译层之前。`test` 是唯一能回答「一个请求真的能完成吗」的命令。

参数：

```bash
slimproxy.exe test -model claude-sonnet-5   # 指定模型
slimproxy.exe test -prompt "1+1=?"          # 换提示词
slimproxy.exe test -timeout 120             # 上游慢时延长
```

---

### `slimproxy version` — 你在跑哪个版本

```bash
slimproxy.exe version
```

```
slimproxy 0.1.0
go        go1.26.4 windows/amd64
CLIProxyAPI v7.2.103
```

第三行是本次构建链接的上游 CLIProxyAPI 版本。报 bug 时把这三行一起贴上——
翻译层的行为由上游版本决定，只报 slimproxy 版本号定位不了问题。

---

### `slimproxy init` — 生成配置

```bash
slimproxy.exe init                          # 写 slimproxy.yaml
slimproxy.exe init -config other.yaml       # 换个文件名
slimproxy.exe init -init-auth-dir /path     # 换凭据目录
slimproxy.exe init -force                   # 覆盖已有的
```

不带 `-force` 时不会覆盖已存在的文件。

---

### 旧写法

`-check`、`-init`、`-routes` 仍然可用，和对应的子命令走同一份代码。
老脚本不用改。

---

## 7. 配置文件

`slimproxy.yaml` 一共 12 项，没有隐藏选项。

```yaml
host: "127.0.0.1"     # 监听地址。"" 表示所有网卡（危险，见下）
port: 8317

api-keys:             # 客户端必须出示的 key，可以有多个
  - "随机生成的串"
allow-unauthenticated: false

auth-dir: "auths"     # 凭据目录，支持 ~ 展开
proxy-url: ""         # 上游走代理，如 "http://127.0.0.1:7890"

request-retry: 3          # 跨凭据的重试上限
max-retry-interval: 30    # 等待冷却凭据的秒数上限
max-retry-credentials: 0  # 一个请求最多试几个凭据，0=不限

debug: false          # 提高日志详细度
request-log: false    # 记录完整请求/响应体（见安全须知）
log-to-file: false    # 日志写文件而非 stdout

models: []            # 模型白名单，空=全部
```

**几个要注意的：**

- **`host: ""` 会监听所有网卡。** 那样局域网里任何人都能访问你的代理。
  除非你清楚自己在做什么，否则保持 `127.0.0.1`。

- **`api-keys` 不能是空的。** 空数组会被拒绝启动——因为底层的认证中间件在没有
  key 的时候是**放行**而不是拒绝，那等于把你的订阅凭据开放给任何能连上端口的人。
  真的想开放，必须显式写 `allow-unauthenticated: true`。

- **`request-retry: 0` 会完全关掉重试。** 上游库对这一项没有默认值，
  写 0 意味着单次遍历凭据、不等冷却。

- **`log-to-file` 在面板模式下会被自动打开。** 因为面板占着 stdout，
  日志必须有别的去处。面板启动时状态行会告诉你路径。

改完配置：文件监听会热重载大部分改动，但改端口需要重启。

---

## 8. 暴露到公网

默认只有本机能访问。想让别的设备用，需要一条隧道。（把访问权交给别人之前，
回看开头的服务条款提醒——那等于共享你的整个账号额度。）

这里用 Cloudflare Tunnel：**不需要公网 IP、不需要开放路由器端口**，
cloudflared 从内网主动连出去。

### 一次性配置

见 [deploy/TUNNEL.md](../deploy/TUNNEL.md)，或者直接跑仓库里的脚本
（两个参数都必填，值来自 `cloudflared tunnel create` 的输出和你自己的域名）：

```powershell
.\deploy\install-tunnel-config.ps1 -TunnelId <创建隧道时打印的 UUID> -Hostname proxy.example.com
```

这一步要做的事：注册 Cloudflare 账号、把域名的 NS 指过去、创建隧道、写配置文件。
只做一次。

### 日常开关

```bash
slimproxy.exe tunnel status     # 现在通不通
slimproxy.exe tunnel up -detach # 后台启动
slimproxy.exe tunnel down       # 停止
```

在面板里更方便：`/tunnel up`、`/tunnel down`。

### 三种状态，别搞混

| 状态 | 意思 |
|---|---|
| 未配置 | 没找到 cloudflared 配置，一次性配置还没做 |
| 已配置，未运行 | 配置在，进程没起 |
| 运行中 | 进程在跑 |

**「运行中」还要看连接数。** 进程活着但边缘连接为 0，公网访问会返回 502——
在任务管理器里看起来一切正常，实际上完全不通。所以：

```
隧道  proxy.example.com   ▲ 运行中但 0 连接    ← 这是坏的
隧道  proxy.example.com   ● 2 连接             ← 这是好的
```

### 暴露之后

用同一个 API key，把 URL 换掉：

```bash
export ANTHROPIC_BASE_URL=https://你的域名
export ANTHROPIC_API_KEY=你的随机串
```

**公网可达之后，`api-keys` 就是你和全世界之间唯一的东西。**
换一个长的随机串，别用默认生成的（虽然它已经是随机的）。

### 日志里的来源地址能信到什么程度

slimproxy 只把**回环**当作可信中继（`127.0.0.1` / `::1`），因为 cloudflared
跑在本机、从回环连进来，是这台机器上唯一有资格转述来源的东西。

后果分三种：

| 请求怎么来的 | 日志记什么 | 能不能伪造 |
|---|---|---|
| 隧道（Cloudflare → cloudflared） | `CF-Connecting-IP` 里的真实公网地址 | 不能 |
| 直连这个端口 | 实际的 TCP 来源地址 | 不能 |
| 本机进程 | `127.0.0.1` | 能，但无意义 |

第三行不是漏洞：能在本机跑进程的人可以直接读 `slimproxy.yaml` 拿走 API key，
伪造一行日志不多给他任何东西。

**如果你把 slimproxy 放在别的机器的 nginx / caddy 后面**，日志记的会是那台反代的
地址，不是最终客户端的——因为那台反代不在可信名单里。这是有意为之：宁可记一个
准确但粗的地址，也不记一个可被任意伪造的精确地址。

`slimproxy log` 里的 `src` 列（`local` / `tunnel` / `remote`）同样按这个规则判定，
**先看连接来源再看请求头**——所以直连的请求自带一个 `CF-Connecting-IP` 也不会被
记成隧道流量。

---

## 9. 出问题了

### 启动就退出，什么都没看到

**最常见**：端口已经被占用了——通常是你之前启动的那个还在跑。

```bash
slimproxy.exe status     # 会告诉你端口被谁占了
```

面板模式下现在会直接告诉你：

```
slimproxy: 无法绑定 127.0.0.1:8317：...
很可能已有一个 slimproxy 在运行。用 "slimproxy status" 查看，停掉它，或用 -port 换一个端口
```

停掉旧的（Windows）：

```bash
taskkill //IM slimproxy.exe //F
```

或者换个端口：

```bash
slimproxy.exe -port 18317
```

### 请求全部失败

按顺序排查：

```bash
slimproxy.exe doctor      # 第一步，永远是这个
slimproxy.exe auth list   # 凭据还有效吗
```

常见原因：

| 症状 | 原因 | 怎么办 |
|---|---|---|
| 401 | 客户端的 key 不对 | 对一下 `slimproxy.yaml` 的 `api-keys` |
| 429 | 上游限流 | 等，或加更多凭据 |
| 502 / 超时 | 上游连不上 | `doctor` 看 upstream-dns 和 upstream-reach |
| 面板显示「无可用凭据」 | 池子空了或全坏了 | `auth list` 看详情，`auth add` 补 |

### 客户端拿到的错误比日志里的简略

这是有意的。上游 SDK 会把 Go 的原始错误文本直接塞进给调用方的响应里，于是一次
连接失败会变成这样发出去：

```
dial tcp 198.18.0.42:443: connectex: A connection attempt failed because...
```

里面有两样不该出门的东西：`198.18.x.x` 说明这台机器走 fake-ip 代理，
`connectex` 说明它是 Windows。代理一旦暴露到公网，这些就是免费的侦察情报。

所以 slimproxy 把发给调用方的失败信息换成了固定分类：

| 客户端看到 | 实际含义 |
|---|---|
| `upstream unreachable: dns resolution failed` | 域名解析失败 |
| `upstream unreachable: connection failed` | 连不上（拒绝／超时／路由不通） |
| `upstream unreachable: tls handshake failed` | TLS 握手失败 |
| `upstream timeout` | 超时 |
| `request canceled` | 客户端在拿到答复前断开了 |
| `upstream unavailable` | 502／503 |

**完整原文没有丢**，只是不再发给调用方——它照常写进应用日志：

```bash
slimproxy.exe log -failed -n 20
```

事件流里记的是**归类**而不是原文（原文是响应正文，事件流不存正文）。
`slimproxy log` 的状态列在没有 HTTP 状态码时显示这个归类：

```
时间              状态      路由             模型            首字   总时长
07-29 14:02:11   connect  claude@you...    sonnet-4        —      35.2s
07-29 14:02:47   dns      claude@you...    sonnet-4        —      1.1s
07-29 14:03:05   429      claude@you...    sonnet-4        —      0.3s
```

`connect` / `dns` / `tls` / `timeout` / `canceled` 这几个值只在**没有状态码**时出现——
传输层的失败根本到不了上游，也就没有 HTTP 状态码可记。以前这一列在这种情况下
一律显示 `err`，等于什么都没说。

上游 Anthropic 自己返回的错误（限流详情、参数错误、overloaded）**原样透传**，
不受影响——那些是调用方需要的信息。

### `doctor` 报 fake-ip 劫持

```
WARN  upstream-dns  api.anthropic.com 解析到 198.18.0.101（fake-ip 段）
```

意思是你本机的代理软件（Clash / mihomo / v2ray 之类）把这个域名劫持了。
`198.18.x.x` 是保留段，不是真实地址。

**后果**：请求可能很慢或者随机失败，长连接会反复断。

**怎么办**：在你的代理软件里让 `api.anthropic.com`（以及隧道用的
`*.argotunnel.com`）直连，或者在 hosts 里固定真实 IP。

### 隧道时断时续

如果 `doctor` 或 `tunnel status` 提到边缘地址在 `198.18.x.x`：

```
⚠ 边缘地址 198.18.0.7 属于 RFC 2544 保留段：隧道流量经由本机代理，
  长连接可能反复断开；让 *.argotunnel.com 直连可消除
```

同上——本机代理软件转发 TCP 但不转发 UDP，而 cloudflared 默认用 QUIC（UDP）。
让 `*.argotunnel.com` 绕过代理即可。

### 日志在哪

```bash
slimproxy.exe check     # 输出里的 "app log" 一行
```

- 默认：stdout（终端里）
- `log-to-file: true` 或**面板模式**：`logs/slimproxy.log`
- 面板模式还有 `logs/stray-stdout.log`，装的是第三方库直接往 stdout 写的东西

---

## 10. 安全须知

**这个进程持有你的订阅凭据。** 拿到它等于拿到你的账号。

### 必须做的

- **`api-keys` 用长随机串。** `init` 生成的已经够了；自己改的话别用短的。
- **暴露到公网前想清楚。** 默认 `127.0.0.1` 只有本机能连，这是安全的默认值。
- **`auths/` 目录别外传。** 里面是 OAuth token 明文。权限已设为 0700。

### 别做的

- **别开 `request-log` 除非你确实要。** 它把请求和响应的**完整 body** 逐字写盘。
  脱敏作用于 header 名和 query 名，而且名字里得含 `authorization`、`api-key`、
  `apikey`、`token`、`secret` 才算数——`Cookie` 不在名单上，明文落盘。任何出现在
  body 里的密钥都会明文落地。

  关掉它是真的关掉，但这是 slimproxy 主动堵的，不是上游的默认行为：上游在
  `request-log` 关闭时仍会对任何 4xx/5xx 强制写全量转储，一个未鉴权的 401
  就足以把调用方的整段对话写上磁盘。见 `proxy/requestlog.go` 的
  `gatedRequestLogger`。
- **别把 `slimproxy.<端口>.effective.yaml` 分享出去。** 那是生成的运行时配置，含所有 key，
  每次启动重写。文件名带端口是有意的：同一目录下跑两个实例时，如果共用一个文件名，
  后启动的会覆盖前一个正在服务的配置——而那个文件是被热重载监听的。
- **别用示例里的 placeholder key。** 程序会拒绝启动——那些值发布在公开仓库里。

### 如果你在自己的部署目录 `git init`

直接复用仓库根的 [`.gitignore`](../.gitignore)——它就是为这套运行时文件写的，
并且带着每条规则的理由（包括为什么二进制模式必须以 `/` 锚定）。自己手写一份
很容易漏掉某个含密钥的生成文件。

---

## 附：命令速查

```bash
slimproxy.exe                      # 启动（终端里带面板）
slimproxy.exe -no-tui              # 启动，只要日志
slimproxy.exe -port 18317          # 换端口启动

slimproxy.exe init                 # 生成配置
slimproxy.exe check                # 校验配置
slimproxy.exe status               # 当前状态（恒退出 0）
slimproxy.exe doctor               # 诊断（有问题退出 1）

slimproxy.exe auth list            # 凭据池
slimproxy.exe auth add claude      # 加凭据
slimproxy.exe auth rm <名字>        # 删凭据

slimproxy.exe tunnel status        # 隧道状态
slimproxy.exe tunnel up -detach    # 后台开隧道
slimproxy.exe tunnel down          # 关隧道

slimproxy.exe routes               # 支持的翻译路由
slimproxy.exe log -since 24h -failed  # 回溯：一天内的失败与当时的状态变化
slimproxy.exe test                 # 发一个真实请求验证链路（消耗配额）
slimproxy.exe version              # 版本与构建信息
slimproxy.exe help <命令>           # 某个命令的用法
```

面板内：`/` 打开命令，`r` 刷新，`q` 退出，`Ctrl+C` 强制退出。
