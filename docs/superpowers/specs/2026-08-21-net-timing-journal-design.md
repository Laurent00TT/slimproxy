# 网速开销三段计时 — 设计

日期：2026-08-21　状态：已与用户确认方案 A；同日终评修订——原「不改 fork」
假设被实测证伪，经用户批准改为含一个 fork 补丁（见下）

## 目标

把每个代理请求的网络开销分解为三段记进 journal 的 req 事件，并在
`slimproxy log` 每行直接显示，让「慢」可以一眼归因：是客户端/隧道上行慢、
上游推理慢，还是回程写出被拖住。此前这只能靠人肉对账访问日志与 journal
的毫秒差才能算出（见记忆 project-slimproxy-retry-reality）。

## 非目标

- 不做定期主动探测（trace/healthz 配对基线曲线）——用户未选，另立项。
- 不测客户端→Cloudflare 边缘那一段：服务端对该段没有观测点。
- 除一个补丁外不改 third_party fork 的行为。原稿在这里断言「fork 已把
  请求 ctx 传进 `HandleUsage(ctx, record)`，旁证是 ResponseHeaders」——
  终评实测为假：各方言 handler 调 `GetContextWithCancel` 时 parent 一律
  传 `context.Background()`，请求 ctx 只贡献取消不贡献值链；而
  ResponseHeaders 走的是种在**新** ctx 上的 holder，从来不是请求 ctx
  继承的证据。因此本设计包含一个经用户批准的 fork 补丁：
  `GetContextWithCancel` 在 parent 为 nil/Background 时改以请求 ctx 为
  parent（登记于 SLIMPROXY_PATCHES.md 第 5/6 条，取消语义不变），守卫为
  forkcheck 的重挂守卫与下面的 wiring 验收测试。

## 三段定义

| 字段 | 定义 | 回答什么问题 |
|---|---|---|
| `up_ms` 上传腿 | middleware 进入（请求头已到）→ `Request.Body` 读到 EOF | 300KB body 经隧道上行花了多久 |
| `ms` / `ttft_ms` 上游段 | 现有字段，不动 | 推理本身多慢 |
| `wb_ms` 回写阻塞 | 包装后 ResponseWriter 每次 Write/Flush 的耗时累计 | 回程隧道/客户端有没有拖住响应 |

「回写腿」刻意不定义为「上游结束→客户端收完」：流式转发是流水线，
两者相减恒近零。累计写阻塞在流水线中依然可测，且直接对应回程压力。

## 架构与数据流

1. **新文件 `proxy/netmeter.go`**：`NetMeterMiddleware` gin 中间件
   （注册方式与 `AccessJournalMiddleware`、`RefusalHookMiddleware` 同一
   router-configurator 通道）。
   - 记录 t0；
   - 包装 `c.Request.Body`：读到 io.EOF 时记下时刻 → `up_ms`；
   - 包装 `c.Writer`：每次 Write/Flush 计时并原子累加 → `wb_ms`；
   - 把 `*NetTimings`（字段全部原子）经
     `c.Request = c.Request.WithContext(context.WithValue(...))` 放进请求 ctx。
2. **`metrics.Sample`** 增加 `UploadMs`/`WriteBlockMs`；
   `Collector.HandleUsage` 停止丢弃 ctx 参数：`netmeter.FromContext(ctx)`
   取出计时结构，原子读快照写入 Sample。
3. **`journal.Event`** 增加 `up_ms`/`wb_ms`（`omitempty`；旧日志文件无此
   字段，读取自然兼容）；`FromSample` 投影新字段。
4. **显示**：`journal/reader.go` 与 log 命令行渲染追加三段（形如
   `传0.9s┊游13.3s┊写0.2s`），任一字段缺失只显示已有段（兼容旧数据）。
   新文案一律 `i18n.T(zh, en)`。
5. **`-stats`**：按现有汇总结构最小追加上传/回写阻塞均值；不改变现有
   统计口径（-stats 的「按显示行数汇总」盲区维持原样，不在本设计修）。

## 已知误差与边界（须写进代码注释）

- usage 发布由流尾触发且经异步队列，最后一笔客户端写可能未计入
  `wb_ms`：原子读快照，少算不多算。
- ctx 值链靠 fork 重挂补丁存活（见非目标节）：upstream 原样会让执行 ctx
  建在 Background 上，三段对所有真实请求静默归零。哨兵两处——forkcheck
  的 TestForkContextReparentGuard（fork 内三条行为守卫）与 proxy 的
  TestNetTimingsSurviveForkContextHop（全链路 wiring 验收测试）。
- FidelityProbe 与 meter 的注册次序是正确性条件：probe 对被采样的
  /v1/messages 请求（每分钟至多一条）在 c.Next() 前 io.ReadAll 整个
  body 再从内存回放，meter 排在它后面时这些请求的 `up_ms` 归零。meter
  必须先于两个抽干者（FidelityProbe、EarlyFlush）注册；gin 语义由
  TestNetMeterOrderAgainstFidelityProbe / TestNetMeterOrderAgainstEarlyFlush
  锁定，proxy.go 里的真实注册顺序由
  TestNetMeterRegisteredBeforeBodyDrainers 锁定。
- ErrorEnvelope 的 writer 在 meter 之下（它注册更早、离网络最近）：它缓冲
  的失败响应体在自己的 finish() 里落网，不经过 meter，`wb_ms` 少算这
  几百字节——少算不多算，可接受。
- 无 usage 记录的入站拒绝（KindReject 路径）不产生 req 事件，不触碰。
- pump 自报事件（notail/clientdrop）不带三段，保持现状。
- 非流式请求走同一路径，天然覆盖。

## 测试

- netmeter 单测：分块慢读 body → `up_ms` 反映延迟；阻塞式 writer 桩 →
  `wb_ms` 反映阻塞；handler panic 时包装不泄漏（panic 上冒、已测计时
  仍可读）。
- **wiring 测试是验收标准**（从风险缓解提级）：真实 gin 请求 ctx（带
  NetTimings）→ fork 真实 `GetContextWithCancel(handler, c,
  context.Background())` → 真实异步 usage manager 派发 → collector 样本
  带三段。实现为 proxy/netmeter_wiring_test.go 的
  TestNetTimingsSurviveForkContextHop，写在 fork 补丁之前、对未打补丁的
  fork 实测为红。假 usage 发布（直接调 HandleUsage）只覆盖 collector
  半程，锁不住 fork 那一跳——终评抓到的正是两半各自全绿而中间断掉。
- reader/显示测试：三段齐全、部分缺失、全缺（旧行）三种渲染。
- `go test -race ./...`：原子字段扛住 HandleUsage 异步读。
- i18n lint 既有测试自动覆盖新文案。

## 风险

- ResponseWriter 包装必须完整透传 Flusher/CloseNotifier/Hijacker 等接口，
  否则 SSE 断流——接口清单照 `proxy/earlyflush.go` 的既有包装抄，
  并与其组合次序无关（任一层测得的写阻塞都包含其下层的网络阻塞）。
- fork 升级时重挂补丁被冲掉（或 `PublishRecord` 改为断开 ctx 传递），
  三段静默消失——forkcheck 重挂守卫 + wiring 验收测试即为此设的哨兵，
  升级流程照 SLIMPROXY_PATCHES.md 走。
