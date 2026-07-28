#!/usr/bin/env bash
# 隧道延迟分层探针 —— 在【访问方的电脑】上运行。
#
# 为什么不能在运行 cloudflared 的那台机器上测：那台机器上 proxy.example.com 走
# 本机代理软件，路径变成「本机→代理→Cloudflare→隧道→本机」，从没离开过本地
# 网络环境，测出来的数字和真实访问者看到的没有关系。
#
# 这个脚本的核心是一次配对测量。同一个主机名下有两个端点：
#
#   /cdn-cgi/trace   Cloudflare 边缘自己应答，请求【不会】进隧道
#   /healthz         穿过隧道回到家里的 slimproxy，机器上只花 ~1ms 处理
#
# 两者唯一的差别就是隧道那一段。相减，就把「你到 Cloudflare 有多快」和
# 「Cloudflare 回源到我家有多慢」这两笔账彻底分开了 —— 这是单看总时间永远
# 得不到的结论。
#
# 用法:
#   bash latency-probe.sh
#   N=20 bash latency-probe.sh                    # 采样 20 次
#   SLIMPROXY_KEY=xxx bash latency-probe.sh       # 额外测真实 LLM 请求的 TTFB
#
# 依赖: curl。awk 用于算数，Windows Git Bash / macOS / Linux 都自带。

set -u

HOST="${HOST:-proxy.example.com}"
N="${N:-10}"
KEY="${SLIMPROXY_KEY:-}"
MODEL="${MODEL:-claude-haiku-4-5-20251001}"

# 中位数比平均值可靠：网络采样里偶发的一次超时会把平均值整个带偏。
# 调用方负责先 sort -n，这里只取中间那个。
median() { awk '{a[NR]=$1} END{if(NR==0){print "n/a";exit} print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}'; }
ms() { awk -v v="$1" 'BEGIN{ if(v=="n/a"){print "  n/a"} else printf "%5.0f", v*1000 }'; }

# 每次采样都用新连接（-H 'Connection: close' 不够，curl 本来就一次一进程），
# 所以测到的是冷启动成本 —— 和真实用户打开一个新会话时看到的一致。
probe() { # $1=path  -> 打印 "ttfb total" 每行一次
  local path="$1" i
  for i in $(seq 1 "$N"); do
    curl -s -o /dev/null --max-time 30 \
      -w "%{time_starttransfer} %{time_total} %{time_appconnect} %{http_code}\n" \
      "https://${HOST}${path}" 2>/dev/null || echo "0 0 0 000"
  done
}

echo "════════════════════════════════════════════════════════════"
echo " 隧道延迟分层探针   host=${HOST}  采样=${N} 次"
echo "════════════════════════════════════════════════════════════"

# ── 0. 你命中的是哪个 Cloudflare 边缘 ───────────────────────────
# colo 是三字母机场码。它决定了你的流量在 Cloudflare 网络里的入口位置，
# 也是解释后面所有数字的前提。
echo
echo "▸ 你的入口边缘"
TRACE=$(curl -s --max-time 15 "https://${HOST}/cdn-cgi/trace" 2>/dev/null)
COLO=$(echo "$TRACE" | awk -F= '/^colo=/{print $2}')
LOC=$(echo "$TRACE" | awk -F= '/^loc=/{print $2}')
MYIP=$(echo "$TRACE" | awk -F= '/^ip=/{print $2}')
echo "  出口 IP   ${MYIP:-?}"
echo "  边缘机房  ${COLO:-?}  (${LOC:-?})"

# ── 1. 配对测量 ─────────────────────────────────────────────────
echo
echo "▸ 采样中 (${N}×2 次请求)..."
EDGE_RAW=$(probe "/cdn-cgi/trace")
TUN_RAW=$(probe "/healthz")

EDGE_TTFB=$(echo "$EDGE_RAW" | awk '{print $1}' | sort -n | median)
EDGE_TLS=$(echo "$EDGE_RAW"  | awk '{print $3}' | sort -n | median)
TUN_TTFB=$(echo "$TUN_RAW"   | awk '{print $1}' | sort -n | median)
TUN_MIN=$(echo "$TUN_RAW"    | awk '{print $1}' | sort -n | head -1)
TUN_MAX=$(echo "$TUN_RAW"    | awk '{print $1}' | sort -n | tail -1)
FAILS=$(echo "$TUN_RAW"      | awk '$4!="200"' | wc -l | tr -d ' ')

TAX=$(awk -v a="$TUN_TTFB" -v b="$EDGE_TTFB" 'BEGIN{printf "%.6f", a-b}')

echo
echo "  ┌─ 到 Cloudflare 边缘 (不进隧道)"
echo "  │    TLS 握手完成      $(ms "$EDGE_TLS") ms"
echo "  │    首字节 TTFB       $(ms "$EDGE_TTFB") ms   ← 你的网络到 CF 有多快"
echo "  │"
echo "  ├─ 穿过隧道到 slimproxy"
echo "  │    首字节 TTFB       $(ms "$TUN_TTFB") ms   (最快 $(ms "$TUN_MIN") / 最慢 $(ms "$TUN_MAX"))"
echo "  │    非 200 响应       ${FAILS}/${N}"
echo "  │"
echo "  └─ 隧道段净开销        $(ms "$TAX") ms   ★ 这就是「暴露到外网」的税"
echo
echo "     slimproxy 本机处理 /healthz 只要约 1 ms，所以上面这个差值"
echo "     几乎全部是 Cloudflare 边缘回源到家里那条链路的成本。"

# ── 1b. 冷连接 vs 热连接 ────────────────────────────────────────
# 上面每次采样都是新进程新连接，含完整 TLS 握手 —— 那是「刚打开客户端」
# 的成本，只付一次。真正决定持续使用体感的是连接复用后的稳态延迟。
# 两个数字差很多，只报一个会误判问题的严重程度。
echo
echo "▸ 冷连接 vs 热连接 (同一连接连发 8 次)"
# -o 必须每个 URL 各给一个：curl 按顺序把 -o 配给 URL，只写一次的话
# 第二个之后的响应体会漏进 stdout，把 -w 的数字冲掉。
WARM_ARGS=""
for i in $(seq 1 8); do WARM_ARGS="$WARM_ARGS -o /dev/null https://${HOST}/healthz"; done
# shellcheck disable=SC2086
WARM_RAW=$(curl -s --max-time 60 -w "%{time_starttransfer} %{num_connects}\n" $WARM_ARGS 2>/dev/null)

COLD=$(echo "$WARM_RAW" | head -1 | awk '{print $1}')
WARM=$(echo "$WARM_RAW" | tail -n +2 | awk '{print $1}' | sort -n | median)

echo "  首次请求 (含 TLS 握手)   $(ms "$COLD") ms   ← 只在建立连接时付一次"
echo "  后续请求 (复用连接)      $(ms "$WARM") ms   ★ 持续使用时每次都付这个"
echo
echo "     如果「后续请求」仍然明显高于「到 CF 边缘」的数字，说明瓶颈不在"
echo "     握手而在链路往返本身 —— 换句话说，加连接池 / keep-alive 救不了。"

# ── 2. 真实 LLM 请求 ────────────────────────────────────────────
# 静态端点测的是链路，但用起来慢不慢取决于推理请求。流式下体感 = TTFB，
# 不是总时长：首个 token 出来之后，后面是逐字流出的。
if [ -n "$KEY" ]; then
  echo
  echo "▸ 真实推理请求 (流式 TTFB, model=${MODEL})"
  for i in 1 2 3; do
    R=$(curl -s -o /dev/null --max-time 120 \
      -H "Authorization: Bearer ${KEY}" \
      -H "content-type: application/json" \
      -d "{\"model\":\"${MODEL}\",\"max_tokens\":16,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"What is 2+2?\"}]}" \
      -w "%{time_starttransfer} %{time_total} %{http_code}" \
      "https://${HOST}/v1/messages" 2>/dev/null)
    echo "  #$i  首字节 $(ms "$(echo "$R" | awk '{print $1}')") ms   全部完成 $(ms "$(echo "$R" | awk '{print $2}')") ms   HTTP $(echo "$R" | awk '{print $3}')"
  done
  echo
  echo "     首字节里含: 你→CF + 隧道回源 + slimproxy→上游 + 模型首 token。"
  echo "     减掉上面的「隧道段净开销」，剩下的才是模型和上游的时间。"
else
  echo
  echo "▸ 真实推理请求  (跳过 —— 设 SLIMPROXY_KEY=<key> 可一并测)"
fi

echo
echo "════════════════════════════════════════════════════════════"
