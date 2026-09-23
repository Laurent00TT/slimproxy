# 本目录是什么

`github.com/router-for-me/CLIProxyAPI/v7` **v7.2.103** 的本地副本，经 slimproxy
根目录 `go.mod` 的 `replace` 指令生效。存在的唯一理由是下面的补丁清单；
除补丁外的一切改动都不属于这里。

拷贝时排除了 assets/docs/examples/README*/Dockerfile 等非构建文件；
`go.mod`、`go.sum`、`LICENSE` 与全部 Go 源码保持上游原样。

# 补丁清单（grep `slimproxy patch` 可全部找到）

上游 claude executor 的 usage 上报没有兜底：流式路径只在扫描到带顶层
`usage` 字段的行（流尾 message_delta）或 scanner 报错时发布记录；非流式
Execute 的 bypass 分支（上游返回 SSE body）同样按行命中才发布。一条在
该行出现之前干净结束的流 = usage/journal 零记录，而访问日志照记 200
（两天 435 条 200 中实锤 2 条）。openai_compat executor 早有
`defer reporter.EnsurePublished(ctx)` 兜底，claude executor 没有——
本补丁只是把这个不对称补齐。`EnsurePublished` 与 Publish/PublishFailure
共享同一个 `once`，发布过即空操作，不会双记。

1. `internal/runtime/executor/claude_executor_stream.go`
   流 goroutine 的 body-close defer 里追加 `reporter.EnsurePublished(ctx)`
   （与 openai_compat_executor.go 流 goroutine 的 defer 形态一致）。

2. `internal/runtime/executor/claude_executor_execute.go`
   `defer reporter.EnsurePublished(ctx)` 声明在 `defer reporter.TrackFailure`
   之前。顺序是语义：defer 后进先出，TrackFailure 必须先执行拿到 once
   （真失败记成失败），EnsurePublished 只兜「什么都没发布」的尾。

3. `internal/runtime/executor/claude_ensure_published_test.go`（新增文件）
   兜底行为的回归测试：假上游吐无尾行的 SSE，断言恰好一条零 token 记录；
   有尾行时断言恰好一条带 token 记录（once 不双记）。上游升级后先跑它。

4. antigravity 凭据脱敏（`internal/auth/antigravity/constants.go`、
   `internal/api/handlers/management/api_tools.go`、
   `internal/runtime/executor/antigravity_executor.go`）：上游把自己的
   Google OAuth client id/secret 硬编码在源码里，GitHub push protection
   会拦下含它们的任何推送。三处置空——本部署不用 antigravity 提供商，
   代价只是那条从未走过的 OAuth 流在 token 交换处失败。升级重建本目录后
   grep `GOCSPX` 必须为零命中。

5. `sdk/api/handlers/handlers.go` `GetContextWithCancel` 执行 ctx 重挂：
   上游所有方言 handler 调它时 parent 都传 `context.Background()`（claude/
   gemini/openai 各文件；唯一例外是 responses websocket 自带 parent），
   执行 ctx 因此不继承请求 ctx 的值链——slimproxy 在请求 ctx 上装的
   `metrics.NetTimings` 到不了异步 usage 发布，三段计时对所有真实请求
   静默为零。补丁：parent 为 nil / `context.Background()` 且请求 ctx
   存在时，改以请求 ctx 为 parent。取消语义不变：原有的 cancel 桥接
   goroutine 干的就是「请求 ctx 取消 → 新 ctx 取消」，直接父子关系
   等价且省掉 goroutine（`requestCtx != parentCtx` 守卫自动跳过它）；
   request-ID 回退逻辑两个分支都仍可达。发布路径的 ctx 消费者已核对
   只读值不问死活（redisqueue / metrics.Collector 只取 value；rpc 插件
   适配器对已取消 ctx 的暴露程度与补丁前相同——cancel 本来就先于
   异步派发）。

6. `sdk/api/handlers/slimproxy_context_reparent_test.go`（新增文件）
   第 5 条的行为守卫：值链继承、取消传播不变、显式 parent 不被覆盖，
   三者各一个测试。forkcheck 的 TestForkContextReparentGuard 把它们接进
   根模块 `go test ./...`；slimproxy 侧另有 proxy/netmeter_wiring_test.go
   走真实 usage manager 全链路断言同一性质（写在补丁之前、对未打补丁的
   fork 实测为红）。上游升级后先跑这两处。

上游的模型目录刷新只在它自己的二进制里启动：`cmd/server/main.go` 的
`startModelCatalogUpdaters` 调 `registry.StartModelsUpdater`（models.json，
启动时拉一次、之后每 3 小时）和 `StartCodexClientModelsUpdater`；SDK 路径
（`sdk/cliproxy`）一处都没调。嵌入 SDK 的进程于是终生只认编进二进制的
目录——构建之后发布的模型对它不存在，直到下次重建。两个拉取器还各自
`&http.Client{}` 裸建客户端，对 proxy-url 一无所知（executor 的 transport
都经 `util.SetProxy`，它们没有）：出口只有代理的机器上，请求正常而刷新
每 3 小时失败一次，症状只是一行 Warnf，进程继续拿内嵌目录服务。

7. `sdk/cliproxy/slimproxy_model_catalog.go`（新增文件）
   `StartModelCatalogUpdater(ctx, client)`：把上游 main 里的两个启动调用
   搬到 SDK 可达的位置——先 `registry.SetModelsHTTPClient(client)`，再启动
   两个 updater。上游 `StartModelsUpdater` 的签名没动，升级不冲突。
   同文件的 `NewModelCatalogClient(cfg)` 用上游自己的 `util.SetProxy` 按
   `ProxyURL` 建客户端：空 proxy-url 时 transport 留 nil，走 Go 默认
   transport，环境变量代理照常生效——与 executor 在该情形下拿到的一样。

8. `sdk/cliproxy/slimproxy_model_catalog_test.go`（新增文件）
   第 7 条的守卫：有 proxy-url 时 transport 选中它；空 proxy-url 时
   transport 为 nil；`StartModelCatalogUpdater` 的启动刷新经手上交的
   client 发出（用只计数、不联网的 transport 观察）。registry 的 updater
   是进程级 once，所以它是本包里唯一允许触发启动的测试。

9. 目录拉取走注入的客户端。`internal/registry/slimproxy_catalog_client.go`
   （新增文件）`SetModelsHTTPClient` + `catalogHTTPClient()`：拉取时读取
   （atomic），nil 恢复默认；`model_updater.go` 与
   `codex_client_models_updater.go` 各一行，把 `&http.Client{Timeout: …}`
   换成 `catalogHTTPClient()`。注入的 client 不带 Timeout 也有界——两个
   拉取器本来就给每个请求包了 modelsFetchTimeout 的 ctx。
   `internal/registry/slimproxy_catalog_client_test.go`（新增文件）三个
   守卫：两个拉取器都经注入的 client 拨号、nil 恢复默认。slimproxy 侧
   `proxy/catalog_wiring_test.go` 另钉 Build 在中继替换**之后**建客户端
   （否则它是进程里唯一不跟着 VPN 切换的出站路径）、Run 用同一个 client
   启动刷新。forkcheck 的 TestForkCatalogClientGuard 把第 8/9 条的六个
   测试接进根模块 `go test ./...`。

Anthropic 的提示缓存默认只活 5 分钟，而且从**上一次读到该前缀的请求开始**
计时——生成回答的时间也算在内。Claude Code 一轮跑过四五分钟（agentic 循环里
很常见），下一轮到达时整段前缀已被逐出，整个对话按全价重写一遍：2026-09-16
一上午实测 18 次约 30 万 token 的全量重建，其中 15 次紧跟在超过 5 分钟的间隔
之后。1 小时的缓存生命期就是为这种场景准备的，客户端却不会在每个断点上都
要求它。executor 是唯一既能看到完整请求体、又知道是哪个客户端发的地方，所以
改写放在这里。改写是**全部断点统一**而不是只补缺：Anthropic 要求长生命期的
断点排在短的前面（tools → system → messages），上游的 `normalizeCacheControlTTL`
会把排在 5m 之后的 1h 一律降回 5m——混着写等于白写；统一写就没有顺序可违反，
这也决定了调用点必须在 normalizer **之前**。只对 claude-cli 生效，因为理由
是这个客户端的轮次长度，而代价是真的（1h 写入按基础输入价 2 倍计，5m 是
1.25 倍）。

10. `internal/config/sdk_config.go` `ClaudeCodeConfig` 加字段
    `CacheTTL`（yaml `claude-code.cache-ttl`，`omitempty`）。slimproxy 的
    `claude-code-cache-ttl` 经 `proxy.Config.build()` 写进这里，随 effective
    config 一起物化、一起热重载。空 = 按客户端发来的原样转发。

11. `internal/runtime/executor/slimproxy_cache_ttl.go`（新增文件）
    `applyClaudeCodeCacheTTL(ctx, cfg, payload)`：User-Agent 以 `claude-cli`
    开头（与 `helps.ShouldCloak` 的 auto 判定同一定义）且 `CacheTTL` 非空时，
    把 tools / system / messages content 里每个 `cache_control` 对象的 `ttl`
    改成该值；没有 `cache_control` 的块不加。无事可做时返回原 slice（字节
    同一性）。调用点各一行：`claude_executor_execute.go` 与
    `claude_executor_stream.go` 里 `enforceCacheControlLimit` 之后、
    `normalizeCacheControlTTL` 之前（带 `slimproxy patch` 注释）。
    count_tokens 路径（`claude_executor_tokens.go`）不动：计数请求不建缓存，
    ttl 对它没有意义。

12. `internal/runtime/executor/slimproxy_cache_ttl_test.go`（新增文件）
    九个守卫：函数级五个（全部断点被改写、改写后能过 normalizer、非
    claude-cli 不动、开关为空不动、无事可做返回原字节）、配置键一个、
    executor 级三个（真 executor 打到假上游，断言上游收到的三个断点都是
    1h——流式与非流式各一，另一个是开关为空时按原样送达的对照）。三个
    executor 级测试是唯一能发现调用点被升级冲掉的守卫；已做变异检查：
    注释掉任一调用点、或把改写改成只补缺，对应测试即红。forkcheck 的
    TestForkCacheTTLGuard 把九个测试接进根模块 `go test ./...`。

路由以模型目录为闸：没有任何 auth 登记过的模型 id，在 handler 里直接回
502 `unknown provider for model`，executor 都不会跑。而目录只在第一次远端
拉取成功之前是内嵌的 models.json——拉取成功即**整份替换**
（`model_updater.go` 的 `modelsCatalogStore.data = parsed`），所以改内嵌
文件加的模型在每次启动后几秒就消失；Anthropic 已发布、router-for-me/models
还没收录的模型于是完全不可达。2026-09-23 的 claude-opus-5-5 正是如此：
Claude Code 的 13 个请求全部是本地 502，一个都没发到上游。配置层没有替代
方案：`oauth-model-alias` 会把上游模型悄悄换成别名的源模型，
`claude-api-key` 的 models 只对 API key 生效（且 UserDefined 会跳过思考
校验、发 budget_tokens）。

13. `internal/registry/slimproxy_claude_extras.go`（新增文件）
    `withSlimproxyClaudeExtras` 在**读取**目录时把固定条目补进 claude 段，
    `lookupSlimproxyClaudeExtra` 是静态查找在各段都落空后的兜底。调用点各
    一行（`model_definitions.go` 的 `GetClaudeModels` 与
    `LookupStaticModelInfo`，带 `slimproxy patch` 注释）。只补缺：远端一旦
    列出同 id，服务的是远端条目。读取时合并而不是写进 store，是为了让
    `detectChangedProviders` 继续拿远端比远端——否则每 3 小时的刷新都会
    报 claude 变更、重注册所有 auth（顺带清掉冷却状态）。
    目前只有一条：claude-opus-5-5。thinking 块是承重的：只有 levels
    （low…max 五级）、不带 min/max——思考层据此把它当 level-only 模型，任何
    客户端的 budget 都改写成 adaptive + output_config.effort；带上 min/max
    （claude-fable-5 的形状）就会把 budget_tokens 发上游，而 Opus 5.5 对
    enabled/budget_tokens 一律 400。zero_allowed 不设，但它**拦不住**显式的
    thinking.type "disabled"：Claude 目标走 `internal/thinking/validate.go`
    的 ModeNone 直通，与目录无关。

14. `internal/registry/slimproxy_claude_extras_test.go`（新增文件）
    四个守卫：目录缺该 id 时注册、静态查找、按渠道列举都恰好各见一条（且
    调用方改返回值不会污染下一次读取）；远端刷新整份替换目录后仍在、没被
    写进 store、未变化的刷新不报变更；远端列出同 id 后让位给远端条目；
    条目形状（level-only、五级、无 min/max、1M/128K）。已做变异检查：去掉
    任一调用点，对应测试即红。forkcheck 的 TestForkClaudeExtrasGuard 把四个
    测试接进根模块 `go test ./...`。
    撤销条件：**内嵌**的 models.json 以正确形状（level-only、含 xhigh）列出
    某个 id 后——也就是下次升级上游、重建本目录之后——从
    `slimproxyClaudeExtras` 删掉它（`TestClaudeExtrasFillCatalogGap` 读的是
    内嵌目录，届时会 Skip 并提示）；条目删空后连同两个调用点一起撤。只有
    远端列出还不够：启动时远端拉取失败，服务的就是内嵌目录。若远端条目带了
    min/max，先别删——那会把 budget_tokens 带回上游，应改成本地条目优先。
    现状：router-for-me/models 于 2026-09-23 02:42（+08）收录了
    claude-opus-5-5，形状正是 level-only、含 xhigh；线上进程 05:21 的刷新后
    服务的是远端条目。本条目此后只在启动拉取失败时兜底。

claude 与 codex executor 经 `helps.NewUtlsHTTPClient` 发请求，而它每个请求
新建一个客户端，所以每个请求都要走一次建连。上游的 `createConnection` 用
`context.Background()` 拨号、`Handshake()` 不设期限、失败不重试。本部署的
链路是 slimproxy → proxy-url（Clash）→ 代理节点 → api.anthropic.com：Clash
立即回 CONNECT 200，之后才以自己的 5s 超时去拨节点，于是节点死掉表现为握手
里的一个 EOF（开始后约 5.0s）；另有握手一挂约 60s、直到外部把它切断。
2026-09-21..23 实测 82 次 ~5.0s 失败、47 次 ~60s 失败，全部发生在任何响应
字节之前，每次都让 Claude Code 重发 0.5-2MB 的请求体；单凭据下 slimproxy
自己不重试（proxy/upstreamretry_test.go 钉的就是「一次」）。建连阶段请求还
一个字节都没写出去——代理只见过 CONNECT，对端只见过 ClientHello 和 HTTP/2
preface——在这里重试不会让上游执行或计费两次；越过这条线（`h2Conn.RoundTrip`
之后）就可能，所以那之后的错误保持上游行为：一次，原样返回。

15. `internal/runtime/executor/helps/slimproxy_utls_setup.go`（新增文件）
    上游 `utls_client.go` 的 `getOrCreateConnection` / `createConnection`
    搬到这里改写，原文件删掉这两个方法、留一段 `slimproxy patch` 注释——升级
    重建若把原版带回来，重复方法直接编译失败，这是有意的报警。改写内容：
    请求 ctx 从 `RoundTrip` 一路传下来；每次建连尝试（拨号含 CONNECT、uTLS
    `HandshakeContext`、HTTP/2 preface）受 `connectAttemptTimeout` 15s 约束
    （要盖住 Clash 的 5s 节点拨号加一次握手；正常建连约 0.6s）。拨号走
    `proxy.ContextDialer`：`proxyutil` 的 HTTP CONNECT 拨号器本来就在 ctx
    结束时切断 CONNECT 的读写，那边不用打补丁；preface 的写不吃 ctx，由
    `context.AfterFunc` 在尝试到期时关掉裸连接兜住。失败即关连接重试，共
    3 次、间隔 1s/3s（包级变量，测试可缩），请求 ctx 一结束立即放弃。重试
    只救得了瞬时故障或 Clash 在两次尝试之间换了节点的情况：节点一直死着、
    Clash 又一直选它时三次都失败，EOF 形态从约 5s 变成约 19s 才失败（挂起
    形态从约 60s 变成约 49s）。每次
    失败以 warn 记下阶段（dial / handshake / h2）、耗时与错误，重试成功记
    info——journal 只留 cause 枚举，这是 ~60s 挂在哪一段的唯一记录。最终
    错误的包装措辞避开 `metrics.CauseFromText` 匹配的词（tls、dial、timeout、
    eof……）并保留错误链（`%w`）：握手 EOF 仍归 connect，尝试到期归 timeout。
    等别人建连的请求改用 channel 等待（`sync.Cond` 不能被 ctx 唤醒），自己的
    ctx 一结束就离开；建连方在任何出口（含 panic）都释放等待者。HTTP/2 连接
    设 `ReadIdleTimeout` 30s / `PingTimeout` 15s：响应中途静默死掉的连接
    （节点消失，FIN/RST 到不了这边）由 PING 探出，而不是一直挂着；Anthropic
    的流自带 SSE ping，静默 30s 发一个 PING 对活连接无害。但 PING 不管有没有
    活跃流都会发（std http2 的 readLoop 照样调度 healthCheck），而每个请求新建
    一个 round tripper，响应结束后留下的连接既不复用、客户端也从不关——补丁前
    靠对端、节点或 Clash 的空闲计时器回收，PING 和 ack 却会把这些按流量计的
    计时器一直重置，孤儿连接于是随请求数累积、永远开着（实测：不设
    `IdleConnTimeout`、PING 间隔缩到 100ms，请求结束 3.5s 后客户端仍没关
    连接，期间 PING 一直在发）。所以同时设 `IdleConnTimeout` 90s（与 net/http
    的 DefaultTransport 一致，包级变量 `h2IdleConnTimeout`）：只在连接上没有
    活跃流时计时，不会切断活着的响应，孤儿连接空闲 90s 后由客户端关掉——顺带
    修掉了原来就有的孤儿泄漏。
    `utls_client.go` 其余改动（均带 `slimproxy patch` 注释）：`pending` 的
    类型、测试用的 `rootCAs` 字段（生产恒为 nil＝系统根证书，与原来相同）、
    `RoundTrip` 传 `req.Context()`。`internal/auth/claude/utls_transport.go`
    里的同形副本没动：它只用于 OAuth 换 token，不在请求路径上。

16. `internal/runtime/executor/helps/slimproxy_utls_setup_test.go`（新增文件）
    九个守卫，全部经真实 `proxyutil.BuildDialer` 走 loopback 上的假 CONNECT
    代理，上游是本地 TLS+HTTP/2 httptest 服务器，不联网：每次尝试到期、3 次后
    放弃（CONNECT 不回 / 握手不回两种卡法）；首次握手 EOF、第二次成功（恰好
    2 次拨号、上游只见 1 次请求、warn 与 info 各一条）；全部 EOF 时错误链与
    journal 分类不变；退避期间请求 ctx 结束即返回且不再拨号；建连之后的错误
    不重试（1 次拨号、上游 1 次），分三种形态：上游 RST_STREAM、请求送达后
    代理关掉连接、请求送达后路径静默由 PING 探出——后两种是死节点在请求中途
    的样子，报的是连接错误而不是 stream error，最容易被人按错误类型「顺手」
    加上重试，所以每种都让首个请求失败、后续请求放行，重试一旦发生就表现为
    上游第 2 次命中加一个成功响应；等待者按自己的 ctx 离开、建连方失败后被
    释放；静默死连接由 PING 探出；尝试期限不延续到已建好的连接（响应比尝试
    期限多流三倍时长仍完整读完，空闲关闭的时限也比停顿短，不许在有活跃流时
    触发）；请求结束后空闲连接在 `h2IdleConnTimeout` 内由客户端关掉（期间
    PING 每 100ms 一次，照样要关）。已做变异检查（逐条注入、见红、还原）：
    去掉每次尝试的期限、拨号不传 ctx、握手既不传 ctx 又去掉兜底、只试一次、
    去掉阶段或恢复日志、退避不看 ctx、建连之后重试（全部错误重试，以及只对
    非 stream error 用 `GetBody` 换新连接重发——后者只让两个连接形态的子测试
    变红）、等待者不看 ctx、失败不释放等待者、去掉 ReadIdleTimeout、包装里写
    "utls"、用 `%v` 断链、拨号错误不带尝试期限、拨号后以 `SetDeadline` 把尝试
    期限留在连接上、建连兜底改成成功后不解除的定时器、去掉 `IdleConnTimeout`、
    把空闲关闭换成建连后固定寿命的 `time.AfterFunc`——对应测试均红。
    forkcheck 的 TestForkUtlsSetupGuard 把九个测试接进根模块 `go test ./...`。

# 升级上游版本的流程

1. `go mod download github.com/router-for-me/CLIProxyAPI/v7@<新版本>`
2. 用模块缓存新版本重建本目录（同样的排除清单），保留本文件与全部
   `slimproxy_*` 文件：`claude_ensure_published_test.go`、
   `sdk/api/handlers/slimproxy_context_reparent_test.go`、
   `sdk/cliproxy/slimproxy_model_catalog{,_test}.go`、
   `internal/registry/slimproxy_catalog_client{,_test}.go`、
   `internal/runtime/executor/slimproxy_cache_ttl{,_test}.go`、
   `internal/registry/slimproxy_claude_extras{,_test}.go`、
   `internal/runtime/executor/helps/slimproxy_utls_setup{,_test}.go`
3. 按上面的清单重打全部补丁（grep 上游新代码确认发布点、
   `GetContextWithCancel` 与两个拉取器的 `client :=` 行没变形、
   两处 `normalizeCacheControlTTL(body)` 调用仍在且第 11 条的调用点
   排在它前面、`GetClaudeModels` 与 `LookupStaticModelInfo` 仍从
   `getModels()` 读 claude 段、`utls_client.go` 删掉上游的
   `getOrCreateConnection` / `createConnection` 并按第 15 条改 `pending`
   类型、加 `rootCAs`、`RoundTrip` 传 `req.Context()`；grep `slimproxy patch`
   核对齐全）
4. 仓库根目录 `go build ./... && go test ./...`。**不要**写
   `go test ./third_party/...`：本目录是嵌套 module，根目录的包通配符
   进不来——那条命令只会打一行 warning 然后 exit 0，一个 fork 测试都
   没跑（评审实测过的假绿）。真正把守卫接进 `go test ./...` 的是根模块
   的 `forkcheck` 包：它 cd 进本目录跑第 3 条补丁的回归测试（-run 只挑
   两个 EnsurePublished 测试——上游自己的 xai TTFT 断言在 Windows 上
   会因时钟粒度偶发翻红）、第 6 条的三个 ctx 重挂守卫、第 8/9 条的六个
   目录客户端守卫、第 12 条的九个缓存 TTL 守卫、第 14 条的四个固定 Claude
   模型守卫、第 16 条的九个 utls 建连守卫，并核对第 5 步的版本同步。每条
   守卫都用 `-v` 跑并逐个核对 `--- PASS:` 行：只看退出码会在测试文件被升级冲掉时假绿——
   `go test -run` 对空匹配打印 `[no tests to run]` 然后 exit 0。
   要手工全量跑上游套件时：`cd third_party/CLIProxyAPI && go test ./...`。
5. slimproxy 根 `go.mod` 的 require 版本号同步改，**并且改本文件第一段
   的版本号**——replace 生效时 require 版本纯属注释，但 `slimproxy
   version` 会把它当作 fork 的基线报出去；forkcheck 的版本同步测试会在
   两处不一致时翻红。

# 撤销条件

上游若合入等价的 EnsurePublished 兜底（可提 PR 引用 openai_compat 的
先例），第 1-3 条可撤。上游若让 SDK 自己启动目录刷新、并让两个拉取器
经 `util.SetProxy` 建客户端，第 7-9 条可撤——slimproxy 侧改为直接调
上游的入口即可。上游若自己提供按客户端改写缓存 ttl 的配置项（可提 PR，
理由与实测数据见第 10-12 条前的说明），第 10-12 条可撤——slimproxy 的
`claude-code-cache-ttl` 改为映射到上游的键即可。上游若让 utls round tripper
的建连跟随请求 ctx、逐次限时、并在请求写出之前重试，且 HTTP/2 连接带 PING
探活与空闲关闭（可提 PR，理由与实测数据见第 15 条前的说明），第 15-16 条
可撤。全部补丁都撤掉后，删除
本目录并移除 go.mod 的 replace 行。
