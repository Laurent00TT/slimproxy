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

# 升级上游版本的流程

1. `go mod download github.com/router-for-me/CLIProxyAPI/v7@<新版本>`
2. 用模块缓存新版本重建本目录（同样的排除清单），保留本文件与
   `claude_ensure_published_test.go`
3. 按上面的清单重打两个补丁（grep 上游新代码确认发布点没变形）
4. 仓库根目录 `go build ./... && go test ./...`。**不要**写
   `go test ./third_party/...`：本目录是嵌套 module，根目录的包通配符
   进不来——那条命令只会打一行 warning 然后 exit 0，一个 fork 测试都
   没跑（评审实测过的假绿）。真正把守卫接进 `go test ./...` 的是根模块
   的 `forkcheck` 包：它 cd 进本目录跑第 3 条补丁的回归测试（-run 只挑
   两个 EnsurePublished 测试——上游自己的 xai TTFT 断言在 Windows 上
   会因时钟粒度偶发翻红），并核对第 5 步的版本同步。
   要手工全量跑上游套件时：`cd third_party/CLIProxyAPI && go test ./...`。
5. slimproxy 根 `go.mod` 的 require 版本号同步改，**并且改本文件第一段
   的版本号**——replace 生效时 require 版本纯属注释，但 `slimproxy
   version` 会把它当作 fork 的基线报出去；forkcheck 的版本同步测试会在
   两处不一致时翻红。

# 撤销条件

上游若合入等价的 EnsurePublished 兜底（可提 PR 引用 openai_compat 的
先例），删除本目录并移除 go.mod 的 replace 行即可。
