# slimproxy

[English](README.md) | **中文**

**让说着不同 API 方言的工具，共用同一份 LLM 订阅。**

把 OpenAI 兼容的编辑器插件、Gemini 格式的脚本和 Claude Code 都指向同一个本地端口，
它们就都能用上同一个上游账号。slimproxy 监听 `127.0.0.1`，在方言之间做翻译，
并且把正在发生的事情实时摆在你眼前。

```
┌─ slimproxy 0.1.0 ──────────────────────────────────────────┐
│ 监听 127.0.0.1:8317  运行 4h12m  rpm 17  ttft 842ms  配额 21% ↗ │
├────────────────────────────────────────────────────────────┤
│ 隧道  proxy.example.com         ● 2 连接                   │
│ 凭据  claude · user@example.com ● 7h24m 后过期             │
│ 诊断  PASS 4 · WARN 1           ▲ 3m12s前                  │
├─ 活跃路由 ─────────────────────────────────────────────────┤
│ openai → claude                        41    812ms     68% │
├─ 最近请求 · 1 进行中 ──────────────────────────────────────┤
│ 12:41:12 ▸   /v1/messages                    8.4s        — │
│ 12:41:07 ok  openai → claude  opus-5        812ms     1.9k │
└────────────────────────────────────────────────────────────┘
   tunnel down    停止本进程启动的 cloudflared
     当前 2 连接 · 停止后 proxy.example.com 立即不可达
 › /tunnel d
```

协议模拟层来自 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)，
原样使用、未做任何修改。slimproxy 添加的是它周围的一切：17 个配置项（而不是约 200 个）、
默认拒绝的入站认证、终端仪表盘、隧道生命周期管理，以及一套「没查清楚就绝不说健康」的诊断。

## 部署之前

> **想清楚你在做什么。** 典型用法是把 API 形态的流量路由到消费级订阅的 OAuth
> 凭据上——多数服务商的条款把订阅限定在其官方客户端内，通过任何代理暴露它
> 都有账号被停用的风险。本项目不为这种用法背书；它假定你已读过服务商条款并
> 自行做出了判断。永远不要把部署分享给你不放心把底层账号交给的人。
>
> **与 Anthropic、OpenAI、Google、Cloudflare 及 CLIProxyAPI 项目均无关联。**
>
> **平台状态**：在 Windows 上开发和运行。Linux 和 macOS 能编译并通过测试，
> 但作者没有在生产环境跑过。

## 五分钟跑起来

需要 Go（版本见 [go.mod](go.mod)）。没有其他依赖——模块从公共代理拉取。

```bash
go build -o slimproxy ./cmd/slimproxy
./slimproxy init
./slimproxy auth add claude
./slimproxy
```

`init` 生成带随机 API key 的 `slimproxy.yaml`、创建凭据目录，并打印接下来该做什么。
`auth add` 走服务商的 OAuth 流程，把凭据存到代理会加载的位置。不带参数运行即开始
服务——在终端里还会打开上面那个面板。

把客户端指向 `http://127.0.0.1:8317/v1`，bearer token 用生成的 key，就完成了。

界面支持中英双语：默认跟随系统语言，配置里写一行 `lang: zh` / `lang: en` 可以钉死。

## 命令

```bash
./slimproxy check      # 校验配置，打印将要运行的内容，然后退出
./slimproxy status     # 报告可观测状态，恒退出 0（适合脚本）
./slimproxy doctor     # 诊断部署问题，有问题退出非 0
./slimproxy auth list  # 凭据池及每一项的状态
./slimproxy tunnel status
./slimproxy routes     # 本构建支持哪些客户端<->上游方言路由
./slimproxy log        # 查询结构化事件日志
./slimproxy test       # 端到端发一个真实请求（消耗上游配额）
./slimproxy version    # 版本与构建来源
```

旧式写法（`-check`、`-init`、`-routes`）仍然可用，走的是同一套命令函数。

## 协议支持

以下是对着真实 Claude 后端端到端实测过的结果（2026-07），区别于「路由表里存在」。
「可用」的意思是：真实请求发过去了、响应形状检查过了——仅此而已。

| 客户端方言 | 端点 | 非流式 | 流式 | 工具调用 | 说明 |
|---|---|---|---|---|---|
| Anthropic | `/v1/messages`、`/v1/messages/count_tokens` | ✅ | ✅ | ✅ | 主路径（Claude Code）。 |
| OpenAI chat | `/v1/chat/completions` | ✅ | ✅ | ✅ 含工具结果回传 | 响应 `id` 保留上游的 `msg_…` 形式，不是 `chatcmpl-…`。 |
| OpenAI Responses | `/v1/responses` | ✅ 结构化 `input` | ✅ | 未测 | **字符串形式的 `input` 会失败**（上游翻译器 bug）：被翻译成空 `messages`，错误以 Anthropic 形状返回。Codex CLI 用结构化形式，可用。 |
| Gemini | `:generateContent`、`:streamGenerateContent` | ✅ | ✅ | ✅ | **`:countTokens` 会失败**（上游翻译器 bug）：构造的计数请求带 `max_tokens`，被 Anthropic 拒绝。 |
| interactions | — | 未测 | 未测 | 未测 | 手头没有说这种方言的客户端。 |

上面两个失败都是上游 CLIProxyAPI 的翻译 bug，任何部署都能观察到，不是这个项目特有的。

运行 `slimproxy routes` 查看你的构建支持的完整矩阵：30 条客户端→上游路由具备请求
和流式响应变换；token 计数只有其中 9 条能表达。

### 需要预先知道的行为

- **`/v1/models` 是静态注册表，不是能力探测。** 它列出上游注册表认识的模型；
  你的凭据仍可能拒绝其中一些（订阅凭据对计划外的模型返回 404）。注册表同时
  也控制放行：不在表里的模型即使凭据能服务也会被拒绝。
- **上游会把 Claude Code 的系统提示**（约 1.4k token）作为客户端模拟的一部分
  注入每个请求。非 Claude Code 客户端会看到助手自称「Claude Code」，且这些
  token 计入用量。
- **拒绝标记因方言而异。** Anthropic 客户端收到原生的 `stop_reason:"refusal"`；
  OpenAI chat 客户端收到 `finish_reason:"content_filter"`；其他方言目前收到
  空补全加日志里一条 WARN（原因见 [proxy/refusal.go](proxy/refusal.go)）。

## 安全姿态

| 请求 | 结果 |
|---|---|
| `GET /healthz` | 200 |
| `GET /v1/models` 无 key | 401 |
| `GET /v1/models` 错误 key | 401 |
| `GET /v1/models` 正确 key | 200 |
| `GET /v0/management/config` | 404 |
| `GET /management.html` | 404 |
| `api-keys` 为空的配置 | 拒绝启动 |
| 有拼错字段的配置 | 拒绝启动，并指出是哪个字段 |

入站认证是**默认拒绝**的：CLIProxyAPI 自己的中间件在没配 key 时会放行所有请求，
slimproxy 选择拒绝启动而不是继承这个行为。环境里有 `MANAGEMENT_PASSWORD` 也会
拒绝启动——这个变量单独就能启用 CLIProxyAPI 的完整管理 API
（`internal/api/server.go:400-404`），且无法从模块外部关掉，所以拒绝是唯一
不用谎报姿态的选择。

发现安全问题？见 [SECURITY.md](SECURITY.md)。

## 文档

| | |
|---|---|
| [docs/GUIDE.md](docs/GUIDE.md) | 使用指南。从零开始，每个命令什么时候用、输出怎么读、出问题怎么查。 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 架构剖析。每个设计决定在什么约束下做出，以及改哪里会出事。 |
| [docs/MIGRATION.md](docs/MIGRATION.md) | 搬机手册。换电脑或再开一台：clone、构建、重新授权，带什么、不带什么。 |
| [deploy/TUNNEL.md](deploy/TUNNEL.md) | Cloudflare Tunnel 的一次性配置（把本机端口暴露到外网，英文）。 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 构建、测试，以及一个改动需要带上什么（英文）。 |

CLI 本身是双语的——见上面的 `lang`。长文档以中文为主，因为这个工具最初的受众
是作者的朋友圈；欢迎任何方向的翻译贡献。

## 包地图

| | |
|---|---|
| `proxy/` | 架在 `sdk/cliproxy.Builder` 之上的最小反向代理。不需要的一律钉死关闭。 |
| `tui/` | 全屏终端仪表盘（bubbletea + lipgloss）。只做展示。 |
| `diag/` | 部署诊断。绝不把没查清楚的问题报告成健康。 |
| `tunnel/` | 操作 cloudflared 子进程：三态状态、启动、停止。 |
| `credentials/` | 检视和管理上游 OAuth 凭据池。 |
| `metrics/` | 请求遥测——已完成的和进行中的——面板在进程内直接读取。 |
| `journal/` | 结构化事件流：每个请求、每次状态变化一个 JSON 对象。 |
| `i18n/` | 界面语言选择。内联双语文案，由 AST lint 测试守门。 |
| `translate/` | `sdk/translator` 之上带类型、大声失败的门面。可独立使用——14 个间接依赖。**不在服务路径上**：CLIProxyAPI 的执行器直接调 `sdktranslator`，那条路径的守卫在 `proxy/untranslated.go`。 |
| `fsperm/` | 把敏感路径限制到当前用户。Windows 上权限位不等于访问控制。 |
| `cmd/slimproxy` | CLI：`serve`、`check`、`init`、`status`、`doctor`、`auth`、`tunnel`、`routes`、`log`、`test`、`version`、`help`。 |

---

以下是参考资料：「slim」是什么和不是什么、面板的行为、translate 门面的契约，
以及一些最好在被惊到之前就知道的配置注意事项。

## 「slim」指什么，不指什么

**指**：配置和运行时表面。一个 17 项的配置文件，而不是约 200 项。没有管理 API、
没有控制面板、没有插件宿主、没有 pprof、没有 Redis 用量队列。不认识的配置项是
错误，不会被静默忽略。入站认证默认拒绝。

**不指**：依赖树和二进制体积。承载上游客户端模拟的 provider 执行器住在
CLIProxyAPI 的 `internal/` 树里。Go 的 internal 包规则让 `sdk/cliproxy.Builder`
成为其他模块触达它们的唯一途径，而构建一个 `Service` 会把 gin、pion/webrtc、
redis 和 lumberjack 全部链接进来，不管那些子系统跑不跑。真要缩小它得 fork
CLIProxyAPI，用依赖树换模拟层上的永久合并负担。

**模拟层按 CLIProxyAPI 出厂的样子原样使用。** 本模块不修改、不扩展、不加固它。

## 面板

不带参数的 `slimproxy` 在一个进程里既服务又画面板。背后没有 HTTP 接口：
面板直接读内存里的收集器，调用的函数和子命令是同一套。

顶栏放着订阅制下真正决定事情的两个数字：五小时配额窗口（带方向箭头）和
prompt cache 命中率——后者是唯一一个坏掉时看不出来的指标，因为回复完全正常，
只有配额在按全价烧。

按 `/` 打开命令：`monitor`、`tunnel status|up|down`、`auth list|rm`、`doctor`、
`routes`、`quit`。提示符上方的面板从不回流——补全列表在框外向下生长，
你正在读的状态在你打字时保持不动。

**破坏性命令在你按 Enter 之前说清代价。** `tunnel down` 不说「停止隧道」，
它说现在有几个边缘连接、哪些主机名会停止应答。然后才问 y/n。

### 它刻意不做的事

- **面板里没有实现 `auth add`。** OAuth 流程要打开浏览器、等待粘贴的授权码；
  在备用屏幕里驱动这个过程，每种失败——没有浏览器、登错账号、授权码过期——
  都表现为面板卡死。命令存在，它指向 `slimproxy auth add <provider>`。
- **`auth rm` 永远不传 `-force`。** 防止删掉最后一个可加载凭据的守卫，无法在
  一个打不了 flag 的面板里绕过。CLI 命令才是操作员必须亲手写出它的地方。
- **不能编辑配置。** `api-keys` 支持热重载，清空它会注销唯一的入站认证
  provider——之后 `AuthMiddleware` 对每个请求都放行，让一个持有活跃 OAuth
  订阅的进程变成开放中继。真实需求由 `init` 覆盖。
- **诊断行是上次 `doctor` 的结论，不是实时上游探测。** 真探测每次刷新都要花
  一次 DNS 查询加一次 TLS 握手。显示一个带时间戳的真实答案，胜过编造一个
  看起来新鲜的。

### 其他要知道的

- **进行中的请求是一行行显示的，不只是个数字。** 已完成请求的遥测来自 SDK 的
  usage record，而它在上游应答之前根本不存在——所以一个中间件单独跟踪请求
  到达，正在跑的请求显示为 `▸` 行，耗时逐秒上涨。上游应答后，这一行原地变成
  正常的一行。
- **输出被重定向时回退到行式日志。** 服务管理器下的 `slimproxy > log 2>&1`
  得到原始行为；面板只在 stdin 和 stdout 都是终端时启动。`-no-tui` 可以在
  终端上强制关掉。
- **面板占用 stdout，日志移到文件。** 备用屏幕是单写者表面，一行 logrus 会落在
  帧的中间。面板模式会打开文件日志（如果原本关着），而不是丢弃这些行——
  `slimproxy check` 会报告它们去了哪。

## 生成的状态文件

启动时，解析完的 CLIProxyAPI 配置写入
`<state-dir>/slimproxy.<端口>.effective.yaml`。它含有机密，且被限制为仅当前用户
可读——为什么单靠 mode 参数在 Windows 上做不到这一点，见 `fsperm/`。文件名带端口
是因为状态目录默认是工作目录：名字固定的话，同一目录里启动的第二个实例会覆盖
第一个正在服务的文件，而那个文件是热重载的。这个文件不是可有可无的记账：
`Builder.Build()` 要求配置路径存在，文件监听器在每次变化时重读该路径，凭据加载
走的是监听器的首轮。文件每次启动都会重新生成——要改就改 `slimproxy.yaml`。

## `translate` 包

`sdk/translator` 很强，但有三个锋利的棱角。这个门面把它们磨掉了。

**1. 探测和翻译的参数顺序相反。** 注册存储为 `responses[client][provider]`。
`Has*ResponseTransformer` 探测读 `responses[from][to]`，所以参数是
`(client, provider)`。但 `TranslateStream`、`TranslateNonStream` 和
`TranslateTokenCount` 读 `responses[to][from]`，所以参数是 `(provider, client)`。

这比查找落空更糟。因为多数路由都注册了反向——`openai→claude` 和 `claude→openai`
都真实存在——顺序写反不会落回透传，而是解析到**反向路由的变换**并静默应用。
`internal/pluginhost/adapters.go:1453,1473` 在调用点手工翻转来补偿。

这个包处处接收 `(Client, Provider)` 并在内部翻转。
`TestProbeAndTranslateTakeOppositeOrders` 钉住了这个不对称，上游哪天把它归一了，
测试会失败，而不是行为悄悄改变。

**2. 流式状态是不检查的 `*any`。** 变换里做 `(*param).(*Params)` 不带 comma-ok，
一个 `*any` 复用到两个配对上会在变换内部 panic。`Stream` 在构造时绑定到一个
`Translator`，状态私有。

**3. 缺失注册不是错误。** 注册表会原样回显请求体，接线接一半的 provider 把未翻译
的字节转发到上游，什么也不记。`New` 返回 `ErrNoTransformer`，除非显式选择透传——
对 `claude→claude` 这类恒等路由这是合理的，执行器对它们走短路而不是翻译。

```go
tr, err := translate.New(translate.Pair{
    Client:   translate.OpenAI,
    Provider: translate.Claude,
}, translate.Options{})

upstreamBody, err := tr.Request(model, clientBody, true)

stream, err := tr.NewStream(model, clientBody, upstreamBody)
defer stream.Finish()
err = stream.ReadFrom(ctx, resp.Body, func(frame []byte) error {
    _, err := fmt.Fprintf(w, "data: %s\n\n", frame)
    return err
})
```

两件门面写明了但修不了的事，因为那是变换自身的契约：

- `Line` 接收一个**原始行**，不是一个 SSE 事件。变换从载荷的 `"type"` 字段恢复
  事件名，丢弃没有 `data: ` 前缀的行。
- `Finish` **不**发终止哨兵。Claude 上游从不合成 `[DONE]`，而
  Gemini/Antigravity/Kimi 会；客户端期待什么因方言而异（`data: [DONE]`、
  一个空行、或什么都没有）。请在拥有 HTTP 响应的那一层发它。

## 值得知道的配置事项

- **`request-retry` 不是失败重试次数——单凭据下它什么都不重试。** 外层循环只在
  「某个凭据正在冷却、且恢复期限落在 `max-retry-interval` 之内」（或 429 带着
  兼容的 `retry-after`）时才会等待并再试。529 和拨号类失败根本不进冷却，
  transient 5xx 的默认冷却（60 秒）又超过建议的 30 秒等待上限，所以单凭据部署
  下每类失败都恰好尝试一次，与这个值无关——对 v7.2.103 实测得出，由
  `TestUpstreamRetry_*` 特征测试钉住。写 0 则整个循环被跳过；CLIProxyAPI
  对它没有默认值。唯一真会发生的重试在这个循环下面一层：到 Anthropic 或
  chatgpt.com 的**建连**失败——拨号、代理的 CONNECT、TLS 握手，请求一个字节
  都还没发——由引擎内部重试，每次限 15 秒、共 3 次、间隔 1 秒和 3 秒，客户端
  一断开就停。这一步不会让上游执行或计费两次；请求一旦发出，之后的失败仍只
  尝试一次。重试救得了瞬时故障，或 Clash 在两次尝试之间换了节点的情况；节点
  一直死着、Clash 又一直选它时，三次都会失败——EOF 形态要约 19 秒才失败（原来
  约 5 秒），挂起形态约 49 秒（原来约 60 秒）。每次建连失败记一行
  `utls: connection setup to … failed in <dial|handshake|h2> phase on attempt
  n/3`——事件流只记归类，卡在哪一段要看这一行。
- **`stream-idle-timeout` 负责切断死掉的流。** 一个流式响应在窗口内（默认 90 秒）一个
  字节都没发，就会被切断，客户端拿到明确的超时错误可以立刻重试。它存在是因为流会在
  中途静默死亡——没有 FIN、没有 RST、没有错误——而上游和本代理都没有读超时，于是
  客户端只能干等到自己的 stall 判定：实测（2026-08-01）每次 5 到 9 分钟。健康的流不会
  安静这么久（上游在思考停顿期间会发 SSE ping）。写 `0` 表示用默认值，负数关闭看门狗。
  在它下面，引擎对到 Anthropic 或 chatgpt.com 的 HTTP/2 连接 30 秒收不到任何帧就发
  PING、15 秒无回应即断开，所以真正死掉的连接即使关了看门狗，也会在最后一帧之后约
  45 秒内失败。只有看门狗能抓住的，是连接还在、上游却不再发东西的流——关掉它，这种
  情况照旧无限期挂起。空闲连接上也会发这种 PING，而每个请求都会留下一条空闲连接
  （每个请求自建客户端，连接从不复用），所以引擎还会关掉连续 90 秒没承载请求的连接；
  否则这些 PING 会让每一条都经 Clash 和节点永远开着。
- **`stream-early-flush` 把流式请求从 Cloudflare 的 100 秒铡刀下买回来。** 挂在 Cloudflare
  隧道后面时，源站约 100 秒内不吐响应头就会被斩成 524（免费/Pro 版不可调）——而上游
  handler 在拿到第一个 chunk 之前一个字节都不写，于是被隧道带宽排队拖住的请求会被边缘
  杀掉，尽管它们最终多半能成功（仅 2026-08-03 一天就有 129 个请求越过 100 秒线）。超过
  阈值（默认 30 秒）后代理提前提交 `200, text/event-stream`，用 SSE keep-alive 注释盖住
  沉默；沉默超过四分钟由看门狗终结。代价只落在慢请求上：预发头之后的失败改以流内 SSE
  error 事件送达，而不是 HTTP 状态码（也没有 `Retry-After`）。阈值内答复的请求——几乎
  全部——逐字节原样。写 `0` 表示用默认值，负数关闭。
- **`claude-code-cache-ttl: "1h"` 让长轮次不再整段重写对话。** Anthropic 的提示缓存
  默认只活 5 分钟，而且从上一次读到该前缀的**请求开始**计时——生成回答的时间也算——
  于是一轮跑过四五分钟的 agentic 循环回来时整段前缀已被逐出，整个对话按全价重写
  （2026-09-16 一上午实测 18 次约 30 万 token 的全量重建）。写 `"1h"` 后代理把
  Claude Code 发来的每个断点都改成 1 小时生命期：是每个，不只是缺 ttl 的那些，因为
  Anthropic 拒绝排在 5m 断点之后的 1h 断点，引擎会把这种混写一律降回 5m。代价是
  1 小时写入按基础输入价 2 倍计（5 分钟是 1.25 倍），五分钟内从不回来的客户端等于
  白付溢价——所以默认留空（断点按原样转发）、只对 Claude Code 生效；`slimproxy init`
  生成的配置直接写 `"1h"`。实现在引擎 executor 里的 fork 补丁，见
  `third_party/CLIProxyAPI/SLIMPROXY_PATCHES.md`。
- **`request-log` 逐字写入请求体。** 脱敏只按 header 和 query 的名字匹配，名字
  里得含 `authorization`、`api-key`、`apikey`、`token` 或 `secret` 才会命中——
  `Cookie` 不含，明文落盘。请求体或响应体里携带的任何凭据都会未脱敏地进日志。
- **`request-log: false` 真的什么都不写。** 上游在请求日志关闭时仍会对任何 4xx
  或 5xx 强制全量落盘，一个未认证的请求就足以把调用方的完整提示词写到磁盘上。
  slimproxy 通过收窄 logger 的方法集堵掉了这条路；见 `proxy/requestlog.go` 的
  `gatedRequestLogger`。
- **运行超过 30 秒后，关停不再优雅。** CLIProxyAPI 在启动时（而不是收到信号时）
  确定关停期限，长时间运行的进程会掐断进行中的流，而不是等它们排空。

完整配置项及注释见 [slimproxy.example.yaml](slimproxy.example.yaml)。

## 悬而未决的决定

`proxy.Config.AllowModel` 目前对 `models:` 做精确匹配，安全但脆——上游名字带日期
后缀，这个代理还会见到追加了思考后缀的名字（`gpt-5.5(high)`）。前缀匹配、
剥后缀、glob，每种都在便利和过度暴露之间做交换。这是真实的访问控制边界，
所以策略在 `proxy/config.go` 里标着 `TODO(you)`，而不是替你猜一个。

## 对着上游构建

`go.mod` 依赖公共模块代理上已发布的 CLIProxyAPI 版本——clone 下来
`go build ./cmd/slimproxy`，别的什么都不需要。`slimproxy version` 打印二进制
构建时对着的确切上游版本；报 bug 时请带上它，因为翻译层的行为是上游的。

## 许可

MIT——见 [LICENSE](LICENSE) 和 [NOTICE](NOTICE)。
