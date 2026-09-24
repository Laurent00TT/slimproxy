# Codex 代理改造与验收

目标：公司电脑通过本机 slimproxy 使用 Codex，连续编程时正确传递工具调用与
会话状态；网络故障有界结束，可诊断，恢复过程不静默丢上下文或盲目重复执行。

## 起点（2026-09-24）

- 散落分支已归入 `main`，包含原 `speed-stability-0923` 的连接保护、上传修复、
  日志与终端显示改进。非空 `models` 配置被拒绝，避免误报尚未实现的白名单。
- 引擎已有 Codex OAuth 登录与刷新、HTTP/SSE Responses、WebSocket 和 compact。
- 根模块 `go test ./...` 现在通过 `TestForkCodexCompatibilityBaseline` 执行
  18 项 Codex 回归测试，覆盖输入、工具参数、流结束、取消计量、压缩和 WebSocket 状态。
  测试使用本地模拟上游，不消耗账号配额，也不能证明真实账号或公网链路可用。
- 真实 OAuth 登录与下表中的本地、公网协议验证已完成。公司端客户端、长任务和
  故障恢复仍待验收；不能据此宣称全部生产场景通过。

## 阶段一：真实使用基线

本机运行 `slimproxy auth add codex` 完成浏览器授权，再启动代理。
公司端使用独立的自定义 provider，指向隧道域名下的 `/v1`，以 slimproxy 入站
key 认证。OAuth 凭据留在本机。模型 ID 从代理目录选择，并以真实请求验证权限。

记录客户端版本、模型、推理强度、传输方式和两端网络。先验证 HTTP/SSE，随后
验证 WebSocket。每项都区分本地地址与公网地址，最后由公司电脑实际运行：

| 场景 | 通过条件 | 当前状态 |
|---|---|---|
| 非流式回复 | 正确结果及明确的完成状态 | 本地通过 |
| 流式回复 | 增量事件有序，结束事件完整，usage 可记录 | 本地、公网通过 |
| 连续多轮 | 前文及工具结果得到保留 | 本地、公网两轮通过；包括 WebSocket 的 previous_response_id |
| 工具调用 | call_id、名称、参数与回传结果关联正确 | 本地、公网 SSE 两轮通过 |
| 并发会话 | 上下文、输出和缓存标识不串线 | 公网两个 WebSocket 会话的不同标记未串线；缓存归属待验 |
| 长推理 | 健康的静默期不会误触发超时 | 待实测 |
| compact | 压缩后可继续完成任务 | 本地、公网 compaction_trigger 通过；独立 compact 接口上游 404 |
| 取消与继续 | 上游及时取消，后续轮次正常 | 公网客户端中止后新请求通过；上游停止时延及取消计量待验 |
| 图片输入 | 图像和文字均到达上游 | 公网合成双色 PNG 与文字问题通过 |

`slimproxy test` 当前只发送一个非流式 Chat Completions 请求。它成功不代表上表
的 Responses、工具循环、compact 或 WebSocket 已通过。

### 2026-09-24 实测记录

代理在 Windows 本机监听 loopback，经本机出站 HTTP 代理访问 ChatGPT；公网
请求从同一台电脑经现有 Cloudflare Tunnel 域名回到代理。**这不是公司网络验收**。
使用独立 Go HTTP/WebSocket 探针，模型 `gpt-6-sol`、推理强度 `low`、
`store:false`，保留 `reasoning.encrypted_content`。本机安装的 Codex CLI 版本为
`0.155.0-alpha.9.2`；这一轮没有用 CLI 执行实际编程任务。

- 非流式返回 `BASELINE_OK`，状态 completed，用时约 10.0s；首次 TLS 握手 EOF
  后，现有的连接建立保护重试成功。
- 本地 SSE 首事件约 2.47s、总计 3.37s，得到完整 completed 事件和 usage。
- 工具两轮校验函数名、JSON 参数和 call_id；回传合成结果 `TOOL_LOOP_OK` 后
  得到正确最终文本。公网两轮总计分别约 4.44s、4.13s。
- WebSocket 在凭据启用 `websockets:true` 后，本地与公网都升级为 101；第二轮
  仅带新增问题及 previous_response_id，正确取回第一轮的标记。公网首轮约
  3.90s，第二轮约 2.47s。响应含上游 `responsesapi.websocket_timing` 事件。
- 公网同时打开两个 WebSocket 会话，各自记住不同标记，第二轮分别用自身的
  previous_response_id 取回原值，没有互串。此检查未验证缓存命中归属。
- 公网发送内存生成的红蓝双色 PNG，模型正确按左右顺序返回 `RED,BLUE`。
- 公网流式长输出在第一个文本增量后取消并关闭连接，随后新请求返回
  `BASELINE_OK`，约 3.56s 完成。只能据此确认后续调用未被阻塞，不能据此证明
  上游停止时延、取消后的计量或 WebSocket 中途断线恢复已经通过。
- 独立 `/responses/compact` 对 `gpt-6-sol`、`gpt-5.5` 均返回上游 404。
  这曾触发模型 12 小时冷却，让后续普通请求变成 503；现已修复并加入回归守卫。
- 通过 `/responses` 的输入追加 `{"type":"compaction_trigger"}`，收到 compaction
  输出项，再将输出作为新上下文发起下一轮，正确找回 `COMPACT_KEEP_4821`。
  本地与公网都通过，公网压缩与续接分别约 10.48s、9.38s。

以上是少量短请求的观测值，不是延迟分位数或稳定性承诺。独立 compact 404 的
修复只隔离失败影响，没有把它改成另一个端点，也没有将失败伪装成成功。
`compaction_trigger` 的协议形式也见 [OpenAI reasoning 文档](https://developers.openai.com/api/docs/guides/reasoning)。

### 公司端接入配置

后续已完成 Astra 的短时稳定性与延迟测量，见
[2026-09-24 基准报告](CODEX_BENCHMARK_2026-09-24.md)。报告包含真实工具循环、
并发 WebSocket、压缩续接，以及本轮修复的验证和部署状态。

把 [配置示例](../deploy/codex.config.example.toml) 中的字段合并到公司客户端的
`~/.codex/config.toml`，将 `base_url` 换成实际隧道地址加 `/v1`。默认模型使用已
实测的 `gpt-6-astra`；low 是本次测试值，可按任务调整。
自定义 provider 使用的 `env_key`、`wire_api`、`supports_websockets` 等字段见
[OpenAI 配置参考](https://learn.chatgpt.com/docs/config-file/config-reference)。

公司端的 `SLIMPROXY_API_KEY` 环境变量填写家里 `slimproxy.yaml` 中的一个入站
`api-keys` 值。不要将家里的 OAuth 文件或刷新令牌复制过去。启动客户端的进程
必须能读取这个环境变量；修改环境变量后，已运行的桌面应用需要重新启动。

家里的 Codex 凭据 JSON 顶层需启用 `"websockets": true`，引擎才会使用上游
WebSocket；本次实测账号已启用。只开启公司端的 supports_websockets 并不保证
上游也使用 WebSocket。若暂时以 SSE 排查，将客户端这个选项设为 false。

先在公司端做短回复、工具调用、两轮上下文及压缩验收，再开始长任务。若客户端
仍调用独立 `/responses/compact` 并收到 404，应记录版本与请求方式，核对其是否
支持新版流式压缩；不要靠删除历史或无限重试掩盖错误。

## 阶段二：流式稳定性

- 为 Codex 分别处理首事件等待、上游空闲与连接失活。现有 Claude 看门狗只包装
  `claude`，不能把同一静默期限直接套给 Codex 的长推理。
- 为 Responses 设计公网等待时的响应头与保活行为。当前 early-flush 只覆盖
  `/v1/messages`；提前提交成功状态后的错误必须按 Responses 协议送达。
- 保活不能重置上游进展计时；慢客户端不能被误判为上游卡死。
- 覆盖上传未完成、客户端取消、正常完成、incomplete、无结束事件 EOF、429、
  5xx、半开连接和代理重启。明确可安全重试的边界，已提交执行的请求不盲目重放。
- 复用已有 HTTP 建连保护并核对其覆盖范围；WebSocket 自有读超时和状态管理，
  需要分别验证，不能假定 HTTP 的保护自动覆盖它。

## 阶段三：WebSocket 与会话恢复

验证会话亲和、推理状态、工具 ID、缓存标识、断线重建与凭据刷新。HTTP 执行器
目前删除 `previous_response_id`，必须实测历史回传与回放机制的组合；WebSocket
则保留该字段，不能将依赖连接状态的请求任意降级到 HTTP。
无法恢复时返回可识别的错误，不能静默丢历史。登录凭据刷新与请求取消都不得
遗留会话锁或连接。

## 阶段四：可观测性

- 按实际返回的数据识别 Codex 配额窗口与重置时间；缺失时显示未知。
- 核对输入、输出、推理、缓存 token 与使用量的发布时机；不伪造缓存写入数据。
- `doctor` 按已加载 provider 检查对应上游；当前上游网络检查偏向 Anthropic。
- 日志关联每轮请求、会话和重试，区分上传、上游处理与下游回压。
- WebSocket 按每次生成追踪进行中状态，不能只追踪 HTTP 升级请求。

## 阶段五：长期运行与发布

先跑不联网的故障回归，再进行公司网络下的真实工作验收。记录成功率、首事件
时间、失败分类与恢复结果，不以 `/healthz` 或模型列表替代端到端验证。
最后完善启动、异常重启、优雅关停、日志轮转及回退流程。

引擎修改遵守 [CONTRIBUTING.md](../CONTRIBUTING.md)：补丁登记到
`third_party/CLIProxyAPI/SLIMPROXY_PATCHES.md`，新增守卫接进 `forkcheck`。
真实测试只使用专门的测试提示；日志不保存 token、完整授权头或公司的源代码。
