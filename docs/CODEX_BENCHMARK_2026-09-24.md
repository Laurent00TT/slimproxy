# Codex / Astra 代理实测：2026-09-24

本轮验证了短请求可用性、延迟、短时连续运行和几种会话功能。正常 QUIC 路径的
48 次 Astra 基准请求全部成功；连续约 5 分 41 秒的本地、公网健康检查各
100 次全部成功。**这些结果不代表公司网络验收、长推理或全天稳定性。**

## 环境与方法

- Windows 本机，slimproxy 基于 `90c9ac3`，cloudflared `2026.8.3`。
- 模型 `gpt-6-astra`，reasoning `low`，`store:false`，无工具的短标记回复。
  48 次基准响应均确认实际返回模型为 `gpt-6-astra`，并校验最终标记和 completed。
- 本地客户端直连 loopback；公网客户端显式经过本机新加坡 VPN 代理，再经
  Cloudflare Tunnel 回到同一台电脑。上游模型访问同样经过配置的出站代理。
- 公网 `/cdn-cgi/trace` 确认出口 `loc=SG`；边缘曾出现 SIN 和 HKG。隧道注册点
  为新加坡。Cloudflare 的 Anycast 选择不能保证每个客户端请求都命中 SIN。
- 19:47–19:49 与 19:53–19:55（北京时间）两批短请求，交替本地/公网，每轮
  分别测 SSE 和 WebSocket。每批每路复用一条 WebSocket，第一轮单列为建连。
- 首事件只计 `response.*`；首文字计 `response.output_text.delta`，不把 rate
  limit 或其他元数据当成文字。总耗时截至完整 completed 事件。
- 样本较少，下表用中位数和最大值；不将短测最大值包装成长期 p95/SLA。
  请求的模型等待、连接复用及网络条件不同，不能直接相减推出“纯代理开销”。

## 延迟

| 路径 | 成功/样本 | 首文字中位数 | 完成中位数 | 完成最大值 |
|---|---:|---:|---:|---:|
| 本地 SSE | 12/12 | 4.16s | 4.76s | 8.78s |
| 公网 SSE | 12/12 | 3.85s | 4.17s | 10.41s |
| 本地 WebSocket，已建立连接 | 10/10 | 1.95s | 2.71s | 4.32s |
| 公网 WebSocket，已建立连接 | 10/10 | 2.36s | 3.13s | 3.91s |

另有本地、公网各两次 WebSocket 首轮建连，也全部成功；包含它们时总耗时最大值
分别为 9.94s、8.32s。首轮比复用连接明显更慢，本轮最有依据的性能建议是保持
WebSocket 会话复用。公司配置模板与本机 Codex 凭据均已支持该路径。

| 不调用模型的网络检查 | 样本 | 中位数 | 观测 p95 | 最大值 |
|---|---:|---:|---:|---:|
| 本地 `/healthz`，复用连接 | 99 | 0.66ms | 1.26ms | 3.40ms |
| 公网 `/healthz`，复用连接 | 99 | 329ms | 769ms | 1,485ms |
| 公网 `/healthz`，重新建连 | 12 | 1,697ms | 3,479ms | 3,479ms |

前两行来自 19:53:33–19:59:14 的连续探测，剔除各自第一条非复用连接；完整
100+100 次均成功。冷连接来自较早的基准批次，耗时包含 VPN CONNECT/TLS 等
建连步骤。观测 p95 使用 nearest-rank，小样本的 p95 可能等于最大值。

## 隧道与受控故障

测试前旧进程日志存在 QUIC 超时与重连；其累计请求错误计数为 56，跨度约一天，
不能将其当成本轮失败数或直接换算可用率。

为比较传输方式，曾临时将隧道改为 HTTP/2。当前网络的 TCP 7844 TLS 握手返回
EOF，隧道未建立；因此没有有效的 HTTP/2 性能对照。切换期间独立监视器记录
50 次公网失败（49 个 502、1 个 530），同期本地 60 次探测全部正常。这是本轮
主动切换引起的中断，不能从测试记录中抹去，也不混入正常 QUIC 性能统计。
标记为 `http2-trial` 的探针样本同样保留，但不参与上表统计。

已恢复原配置并重新接通 QUIC。恢复后连续检查通过；20:04 左右，隧道由
2 条连接自行恢复到 4 条，该进程的请求错误计数仍为 0。

不建议在这条网络上强制 HTTP/2，也不应将某个边缘 IP 当成稳定的“新加坡节点”。
Cloudflare 官方参数只提供默认 global 与 `us` 的 region 选择，没有 `sg`；
`auto` 的协议行为和区域参数见
[Cloudflare run parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/)。

## 功能与修复

公网 Astra 的以下功能均已实测通过：

- 工具调用两轮：函数名、参数和 call_id 校验成功，回传后得到 `TOOL_LOOP_OK`。
- 两个并发 WebSocket 会话：分别用 previous_response_id 取回不同标记，未串线。
- `compaction_trigger` 产生 compaction 项，续接后正确取回 `COMPACT_KEEP_4821`。

本轮修复两处实际问题：

1. `tunnel status` 将 connector 进程数误当边缘连接数。现解析 EDGE 列中的
   `2xsin09, 1xsin12` 等聚合计数；无法识别的格式仍报未知。修复版实际显示
   4 条，与 cloudflared HA 指标一致。
2. Codex SSE 取消和完成但缺少 usage 时不发布请求记录。现补齐取消失败记录或
   无 token 数的完成记录，保留 once 防重。独立 loopback 测试实例对真实 Astra
   在首段文字后取消，日志恰好一条 `cause=canceled`；随后请求正常返回
   `BASELINE_OK`，约 3.65s。真实上游停止耗时仍未独立测量。

修复版通过 `go build`、`go vet ./...`、`go test ./...`。两处回归均做变异验证：
恢复旧行为会失败。Codex 根模块基线由 17 项增加至 18 项，新增守卫包含四种
流结束场景。未改 token、重试或冷却策略，也未将取消请求当成成功。

运行状态：修复已编译为 `slimproxy.next.exe`，并在独立端口验证。替换生产进程的
操作被自动审批拦截，故本轮结束时生产代理仍运行旧版；隧道已恢复 QUIC。

## 下一步优先级

1. 保持已启用的 WebSocket 复用；在公司电脑重测同样的四种请求路径。
2. 为 Codex 单独完善长推理的首事件等待、空闲检测与半开连接恢复。现有 Claude
   early-flush/watchdog 不能直接照搬，否则可能误杀健康的静默推理。
3. 针对 VPN 到 Cloudflare 的 UDP/TCP 7844 路径继续定位冗余连接抖动；已有
   回退配置保留，不凭一次短测修改 VPN 全局路由。
4. 增加真实编程长会话、凭据刷新、断网重连与公司网络长时间运行验收。本轮短
   标记测试没有覆盖大上下文、长时间工具执行或高推理强度。

本机原始样本保存在忽略的 `logs/bench-20260924.jsonl`，基准探针快照为
`logs/bench-20260924.go`；独立实例日志位于 `logs/bench-runtime/`。这些本机文件
不随 Git 发布。统计只纳入 `quic-baseline`、`quic-restored`、`quic-soak` 相应
阶段；原始记录保留受控故障。探针仅使用合成提示，凭据只在内存中用于认证，
不写入测量输出。独立实例配置和有效配置含入站 key，继续保留在被忽略的目录。
