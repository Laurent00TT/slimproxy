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

# 升级上游版本的流程

1. `go mod download github.com/router-for-me/CLIProxyAPI/v7@<新版本>`
2. 用模块缓存新版本重建本目录（同样的排除清单），保留本文件与全部
   `slimproxy_*` 文件：`claude_ensure_published_test.go`、
   `sdk/api/handlers/slimproxy_context_reparent_test.go`、
   `sdk/cliproxy/slimproxy_model_catalog{,_test}.go`、
   `internal/registry/slimproxy_catalog_client{,_test}.go`
3. 按上面的清单重打全部补丁（grep 上游新代码确认发布点、
   `GetContextWithCancel` 与两个拉取器的 `client :=` 行没变形；
   grep `slimproxy patch` 核对齐全）
4. 仓库根目录 `go build ./... && go test ./...`。**不要**写
   `go test ./third_party/...`：本目录是嵌套 module，根目录的包通配符
   进不来——那条命令只会打一行 warning 然后 exit 0，一个 fork 测试都
   没跑（评审实测过的假绿）。真正把守卫接进 `go test ./...` 的是根模块
   的 `forkcheck` 包：它 cd 进本目录跑第 3 条补丁的回归测试（-run 只挑
   两个 EnsurePublished 测试——上游自己的 xai TTFT 断言在 Windows 上
   会因时钟粒度偶发翻红）、第 6 条的三个 ctx 重挂守卫、第 8/9 条的六个
   目录客户端守卫，并核对第 5 步的版本同步。每条守卫都用 `-v` 跑并逐个
   核对 `--- PASS:` 行：只看退出码会在测试文件被升级冲掉时假绿——
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
上游的入口即可。全部补丁都撤掉后，删除本目录并移除 go.mod 的 replace
行。
