# slimproxy 搬机手册

换电脑，或者在第二台机器上再跑一份。旧机可以照跑不停。

**什么都不用从旧机拷**：clone 仓库 → 构建 → `init` 生成配置 → 改两处出口 →
重新授权 → 跑一遍验证阶梯。整个过程十分钟上下，其中授权两分钟。

---

## 目录

1. [先记住一条：一切路径按工作目录解析](#1-先记住一条一切路径按工作目录解析)
2. [第一步：拿到可运行的程序](#2-第一步拿到可运行的程序)
3. [第二步：带什么，不带什么](#3-第二步带什么不带什么)
4. [第三步：生成配置，改两处](#4-第三步生成配置改两处)
5. [第四步：凭据——新机重新授权，不拷文件](#5-第四步凭据新机重新授权不拷文件)
6. [第五步：启动前体检](#6-第五步启动前体检)
7. [第六步：验证阶梯](#7-第六步验证阶梯)
8. [日常运行](#8-日常运行)
9. [禁忌](#9-禁忌)
10. [失败指纹](#10-失败指纹)
- [附：Cloudflare 隧道（多半不需要）](#附cloudflare-隧道多半不需要)
- [附：核对清单](#附核对清单)

---

## 1. 先记住一条：一切路径按工作目录解析

配置文件（默认 `slimproxy.yaml`）、凭据目录（`auth-dir`，默认 `auths`）、日志
（`logs/`）、事件日志（`logs/events/`）、运行时生成的 `slimproxy.<端口>.effective.yaml`，
凡是相对路径，都相对**启动时的工作目录**取绝对值。没有「exe 所在目录」的回退，
也没有「用户目录」的回退。

所以新机器上的形态就是一个目录，东西平铺进去：

```
D:\slimproxy\
├── slimproxy.exe
├── slimproxy.yaml
├── auths\            ← auth add 写进来
└── logs\             ← 运行后出现
```

快捷方式、计划任务、服务管理器的「起始位置 / 工作目录」**必须**指到这个目录。
指错了不会报错：服务照常启动，在错误的位置**静默**建出一套空的 `auths\` 和
`logs\`，然后每个请求都因「无可用凭据」失败。这是搬机后最常见的一种坏法，
见 [失败指纹](#10-失败指纹)。

---

## 2. 第一步：拿到可运行的程序

### 路线 A：clone 后自己构建（推荐）

仓库自包含：CLIProxyAPI 的 fork 就在 `third_party/CLIProxyAPI`，根 `go.mod` 用
`replace` 指过去，不需要额外拉任何私有依赖。新机器装 Go 1.26 或更新：

```bash
git clone https://github.com/Laurent00TT/slimproxy.git
cd slimproxy
go env -w GOPROXY=https://goproxy.cn,direct    # 国内网络；海外可略
go build -o slimproxy.exe ./cmd/slimproxy
```

要验证构建，在**仓库根目录**跑 `go test ./...`。**不要**写
`go test ./third_party/...`：fork 是嵌套 module，那条命令只打一行 warning
然后退出 0，一个 fork 测试都没跑。根模块的 `forkcheck` 包会把 fork 的守卫
测试接进 `go test ./...`，那才是真的跑了。

构建好的 exe 拷到部署目录（比如上面的 `D:\slimproxy\`），仓库目录本身留着
以后升级用。

### 路线 B：从旧机拷 exe（装不了 Go 时）

`slimproxy.exe` 是纯 Go 构建，不依赖 cgo，拷到同架构（windows/amd64）的任何
Windows 机器就能跑。

两点注意：

- **先在旧机重新构建再拷**，确保拷的是当前代码而不是几周前的某次构建。
  `slimproxy.exe version` 会打印构建信息，对不上就重建。
- Windows 不能覆盖正在运行的 exe。旧机的代理还在跑的话，构建到另一个文件名：
  `go build -o slimproxy.new.exe ./cmd/slimproxy`。

---

## 3. 第二步：带什么，不带什么

| 旧机上的东西 | 处置 | 为什么 |
|---|---|---|
| `slimproxy.exe` | **不拷** | 新机自己构建（路线 A）；只有装不了 Go 才拷，且要先重建 |
| `slimproxy.yaml` | **不拷** | 新机 `init` 生成后照第三步改。旧机这份写着旧机的绝对路径，还含 api-key |
| `auths\*.json` | **不拷** | 旧机继续跑就绝不能共用——见第四步。新机 `auth add` 拿独立授权 |
| `slimproxy.*.effective.yaml` | **不带** | 每次启动重新生成，嵌着旧机绝对路径和全部密钥 |
| `logs\`、各种 `.log`、旧 exe 备份 | **不带** | 死重，全部 gitignored |
| `cloudflared.pid.json` 之类的进程状态 | **不带** | 旧机的进程状态，带过去只是噪音 |
| `logs\events\`（事件日志） | 可选 | 只在想保留请求历史时带；`journal-days` 的保留期反正会清 |

一句话：**除了「我想留着的历史事件」，什么都不带。** 新机上真正属于你的东西
只有两样——配置里的几行出口设置，和一次两分钟的授权。

---

## 4. 第三步：生成配置，改两处

在部署目录里：

```bash
.\slimproxy.exe init
```

它生成 `slimproxy.yaml`，随机一个 api-key，建好 `auths\`。然后改两处。

### ① `auth-dir`：改成相对路径

`init` 写进去的是 `auths` 的**绝对路径**（在哪个目录跑的 init 就是哪个目录）。
它能用，但把它改成相对的：

```yaml
auth-dir: "auths"
```

相对路径按工作目录取绝对值，从此这份配置在哪台机器、哪个目录都能用，
搬第三次机的时候不用再改。

### ② `proxy-url` / `proxy-fallback-direct`：按新机的出口改

新机器怎么出网，决定这两行怎么写：

| 新机的出口 | 写法 |
|---|---|
| 有本地 HTTP 代理（Clash 之类，系统代理模式） | `proxy-url: "http://127.0.0.1:<它的端口>"` |
| 有时系统代理、有时 TUN 全局接管，换着用 | 上一行再加 `proxy-fallback-direct: true`：代理端口没监听时逐连接改走直连，切换不用重启 |
| 只有 TUN 型 VPN 全局接管 | `proxy-url: ""` 直连即可，TUN 会透明接管 |
| 什么出口都没有 | **先解决出口。** 从被拒绝的地区直连上游会在边缘拿到 403，不是配置问题 |

`proxy-fallback-direct` 要求 `proxy-url` 是本机环回的 `http://` 地址，
`check` 会在不满足时拒绝。

> **`proxy-url` 不能热应用。** 改它必须重启 slimproxy：上游 HTTP 客户端在
> 启动时把代理定型，改完不重启，进程仍走旧路径。另外 `auth add` 的 token
> 交换也走同一个 `proxy-url`，所以先把出口配好，再做第四步。

改完跑一下：

```bash
.\slimproxy.exe check
```

它不联网，只校验配置并打印解析结果——看一眼「凭据目录」那行指向的是不是
你期望的位置。

---

## 5. 第四步：凭据——新机重新授权，不拷文件

```bash
.\slimproxy.exe auth add claude
```

浏览器打开授权页，两分钟。写入 `auths\` 后运行中的代理会热加载，不用重启。
同一账号在多台机器上各自授权是官方客户端自己的日常用法（每台电脑登录一次、
互不影响），拿到的是各自独立的 refresh token。

### 为什么不能直接拷旧机的凭据文件

刷新代码是「轮换就绪」的：刷新响应若带来新的 refresh token，就替换存储值并
写回文件。旧机的代理持续在跑、周期性预刷新；两台机器共用同一个 refresh token，
一旦服务端轮换并吊销旧的，输家在下一次预刷新时拿到 unauthorized，凭据被挂起。
这个风险从代码无法排除，而重新授权只要两分钟。

如果旧机已经彻底停用，拷文件不会撞上轮换，但也没省下什么。

### `auth add` 的三个硬条件

1. **本机的 54545 端口空闲。** Claude 的回调地址是常量 `localhost:54545/callback`，
   `-callback-port` 换端口会让授权服务器把你送到没人听的地方。
2. **5 分钟内完成。** 回调等待有超时；超时了重跑一次即可。
3. **出口已配好。** token 交换走 `proxy-url`；`proxy-fallback-direct` 开着时，
   代理端口没监听则直连，命令会把走了哪条路打印出来。

浏览器被管控、或者想在手机上完成授权：

```bash
.\slimproxy.exe auth add claude -no-browser
```

它打印授权链接，你在任何浏览器里打开。授权后浏览器会跳到
`http://localhost:54545/callback?code=...`——在另一台设备上这个页面当然打不开，
把地址栏里**整条 URL** 复制回终端粘贴（终端会提示 `Paste the Claude callback URL`），
流程照常完成。

### 两台机器共用的是同一份配额

5 小时窗口和短窗限流都按账号算。两台机器的会话同时高并发，会触发一阵 429——
那不是用量到顶，是并发冲爆了短窗限流，错开一点就好。

---

## 6. 第五步：启动前体检

- **环境变量。** 新机器上若存在 `MANAGEMENT_PASSWORD`，或任何 `PGSTORE_*` /
  `GITSTORE_*` / `OBJECTSTORE_*` 变量，slimproxy **拒绝启动**（设计如此：前者
  会悄悄打开上游的完整管理 API，后者会让上游去别处找凭据而加载零个）。
  `WRITABLE_PATH` 会把上游的日志落点改到别处。PowerShell 里 `Get-ChildItem env:`
  扫一眼。
- **端口。** 8317（服务）空闲，授权期间 54545 空闲。`slimproxy status` 会说
  8317 被谁占着。
- **公司网络。** 办公网络常见的透明网关（返回 HTTP 200 拦截页那种）会干扰
  上游回程。搬到这类网络后如果 `test` 稳定失败而配置和家里一样，先怀疑网关，
  换个网络（手机热点）验证一次再改配置。
- **模型目录拉取。** 代理启动时和之后每 3 小时从
  `raw.githubusercontent.com`（备用 `models.router-for.me`）拉一份模型目录，
  走的是与上游请求相同的出站路径（`proxy-url` / 中继）。拉不到只是保留编进
  二进制的那份目录，不影响启动，代价是构建之后新发布的模型要等下次拉取成功
  才认得。启动后看一眼：

  ```powershell
  Select-String "model refresh" logs\slimproxy.log
  ```

  `completed from ...` 是成功，`fetch failed from all URLs` 是没拉到。

---

## 7. 第六步：验证阶梯

依次跑，每一级通过再跑下一级：

| | 命令 | 验证什么 | 盲区 |
|---|---|---|---|
| 1 | `.\slimproxy.exe check` | 纯配置校验，不联网。未知键是硬错误，立刻抓住配置与二进制版本不匹配 | 不碰运行时 |
| 2 | `.\slimproxy.exe doctor` | 六项：监听端口、凭据、配置字段、上游 DNS、上游可达、隧道 | 不校验凭据有效期；「listen-port 空闲」在日常巡检里恰恰是代理已死的信号 |
| 3 | `.\slimproxy.exe auth list` | 凭据被识别、状态正常 | 不发请求 |
| 4 | `.\slimproxy.exe test` | **唯一的端到端证明**：发一个真实请求（消耗少量配额，一两千 token） | — |

第 4 级通过，迁移完成。

---

## 8. 日常运行

**启动。** 终端里直接运行带全屏面板；服务方式运行加 `-no-tui`：

```bash
.\slimproxy.exe            # 面板
.\slimproxy.exe -no-tui    # 只要日志（输出被重定向时自动生效）
```

面板会把日志改写到 `logs\slimproxy.log`（面板占着 stdout），状态行会告诉你路径。

**计划任务 / 开机自启。** 「起始位置」设成部署目录（见第 1 节），程序参数用
`-no-tui`。用 `-config <路径>` 指定配置文件时，其他相对路径**仍然**按工作目录
解析，不按配置文件所在目录。

**本机客户端指过来。** 新机上的代理只给本机用的话，客户端设
`ANTHROPIC_BASE_URL=http://127.0.0.1:8317`（OpenAI 兼容用 `/v1`），不需要隧道。

**回溯。** 出过什么事，事件日志里都有：

```bash
.\slimproxy.exe log -since 24h -failed
```

---

## 9. 禁忌

- **不要两台机器共用一份 `auths\`**（拷文件、网盘同步、共享目录都算）。
  原因见第四步。
- **不要两台机器跑同一个 Cloudflare 隧道**。见附录。
- **不要用 `go test ./third_party/...` 当验证**。它什么都没跑。
- **不要覆盖运行中的 exe**。Windows 会拒绝；构建到新文件名，停掉再换。
- **不要把 `*.effective.yaml` 带走**。它含全部密钥，且每次启动重新生成。
- **不要在 `auth add` 时换回调端口**。回调地址是常量。

---

## 10. 失败指纹

| 症状 | 原因 | 处置 |
|---|---|---|
| `check` 报未知键 | 配置与二进制版本不匹配 | 用这台机器的二进制 `init` 重新生成，把旧值搬过去 |
| 服务起来了，`auth list` 是空的，每个请求「无可用凭据」 | 工作目录不对，在别处建了一套空 `auths\` | 看 `check` 打印的凭据目录；修快捷方式 / 计划任务的起始位置 |
| `auth add` 打不开回调页、或报端口被占 | 54545 被占用 | 释放端口；不要换端口 |
| `auth add` 卡在 token 交换 | 出口没配好；交换走的就是 `proxy-url` | 先做第三步 |
| 一切正常但请求 403 | 从被拒绝的地区直连上游 | 配出口 |
| `doctor` 全 PASS，`test` 稳定失败，家里同配置正常 | 公司透明网关干扰回程 | 换网络验证；不是配置问题 |
| 日志 `model refresh: fetch failed` | 目录拉取走不出去 | 不影响启动；确认 `proxy-url`；只是暂时没有新模型 |
| 突然一阵 429 | 两台机器同账号并发冲爆短窗限流 | 错开使用；不是用量上限 |
| 启动时报环境变量 | `MANAGEMENT_PASSWORD` / `PGSTORE_*` 等存在 | 清掉再启动 |
| 一台机器的凭据突然 unauthorized 被挂起 | 两台共用了同一份凭据文件，refresh token 被轮换 | 各自 `auth add`，删掉共用的那份 |
| 改了 `proxy-url` 没生效 | 不能热应用 | 重启 slimproxy |

---

## 附：Cloudflare 隧道（多半不需要）

旧机不停，隧道就留在旧机；新机上的代理只给本机用，客户端直接指
`http://127.0.0.1:8317`，还省掉绕边缘节点那段延迟。**新机不要起 cloudflared**：

- 隧道身份在 `%USERPROFILE%\.cloudflared\`（`cert.pem`、`<UUID>.json`、
  `config.yml`），不在仓库里。不拷它，新机就起不了这条隧道——这正是想要的。
- **两台机器同时跑同一个隧道 UUID，Cloudflare 视为高可用副本**，外部请求会
  随机分到新机，穿新机所在的网络。
- cloudflared 默认走 QUIC/UDP 到 `*.argotunnel.com`，须绕开本地代理直连；
  网络封 UDP 出站时它会退到 HTTP/2，但如果连直连都被塞进代理，隧道起不来。
  旧机上为此打的静态路由之类的补丁是旧机网络的事，不用照搬。

真要在新机上暴露到公网，按 [deploy/TUNNEL.md](../deploy/TUNNEL.md) 新建一条
隧道，而不是复用旧的。

---

## 附：核对清单

- [ ] 旧机的 slimproxy 与 cloudflared 原样不动（如果旧机继续跑）
- [ ] 新机 clone + `go build`（Go ≥ 1.26；国内设 GOPROXY），或拷一份**刚重建**的 exe
- [ ] 部署目录建好，exe / `slimproxy.yaml` / `auths\` 平铺
- [ ] `init` 之后：`auth-dir` 改相对；`proxy-url` / `proxy-fallback-direct` 按新机出口改；`check` 通过
- [ ] 扫环境变量：无 `MANAGEMENT_PASSWORD` / `PGSTORE_*` / `GITSTORE_*` / `OBJECTSTORE_*` / `WRITABLE_PATH`
- [ ] 快捷方式 / 计划任务的起始位置指向部署目录
- [ ] `auth add claude` 成功，`auth list` 能看到凭据
- [ ] `check` → `doctor` → `auth list` → `test` 依次通过
- [ ] `Select-String "model refresh" logs\slimproxy.log` 看到 `completed`
- [ ] 本机客户端 `ANTHROPIC_BASE_URL` 指向 `http://127.0.0.1:8317`
