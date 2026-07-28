#!/usr/bin/env python3
"""Argo Smart Routing 开通前后的 A/B 对比。

为什么需要它：Argo 按用量计费，而它只优化「用户边缘 → 隧道所在边缘」那一段，
优化不了「隧道边缘 → 家里」那 182ms。到底值不值必须用数据说话，而一旦开通，
「开之前」的数据就再也采不到了 —— 所以先跑一次 before。

测量手法是配对相减。同一主机名下：

    /cdn-cgi/trace   Cloudflare 边缘自己应答，请求不进隧道
    /healthz         穿过隧道回到家里，slimproxy 上只花约 1ms

同节点、同 TLS、同时间窗内跑两遍，相减得到的就是纯隧道开销。直接看总时间
没有用 —— 各节点自身到 CF 的距离差了一个数量级，混在一起噪声盖过信号。

用法:
    python argo-ab-test.py --label before     # 开通 Argo 之前跑
    python argo-ab-test.py --label after      # 开通并等几分钟后跑
    python argo-ab-test.py --compare before after

依赖: 只用标准库。check-host.net 的公开 API，无需注册。
"""

import argparse
import json
import os
import sys
import time
import urllib.parse
import urllib.request

HOST = os.environ.get("PROBE_HOST", "proxy.example.com")
API = "https://check-host.net"
SNAP_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "argo-snapshots")


def _get(url):
    # 必须伪装 UA：check-host 对 urllib 的默认 "Python-urllib/3.x" 直接回 403。
    req = urllib.request.Request(url, headers={
        "Accept": "application/json",
        "User-Agent": "Mozilla/5.0 (compatible; slimproxy-latency-probe)",
    })
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)


def submit(path, nodes=25):
    target = urllib.parse.quote(f"https://{HOST}{path}", safe="")
    d = _get(f"{API}/check-http?host={target}&max_nodes={nodes}")
    if not d.get("request_id"):
        raise RuntimeError(f"check-host 拒绝了请求: {d}")
    return d["request_id"]


def collect(request_id, wait=25):
    """轮询直到结果稳定。

    check-host 的节点是陆续回填的，过早读取会把还没回来的节点当成失败。
    连续两次拿到相同的完成数才认为收敛。
    """
    prev, stable = -1, 0
    for _ in range(wait):
        time.sleep(2)
        d = _get(f"{API}/check-result/{request_id}")
        done = sum(1 for v in d.values() if v is not None)
        if done == prev and done > 0:
            stable += 1
            if stable >= 2:
                break
        else:
            stable = 0
        prev = done
    out = {}
    for node, v in d.items():
        if not v or v[0] is None:
            continue
        r = v[0]
        # r = [success, time, message, http_code, ip]
        if r[0] == 1 and r[1]:
            out[node.split(".")[0]] = round(r[1] * 1000)
    return out


def measure(label):
    print(f"▸ 采集 {label} 快照  (host={HOST})")
    print("  提交 /cdn-cgi/trace  (边缘直接应答，不进隧道)...")
    id_edge = submit("/cdn-cgi/trace")
    time.sleep(3)
    print("  提交 /healthz        (穿过隧道回家)...")
    id_tun = submit("/healthz")

    print("  等待各节点回填...")
    edge = collect(id_edge)
    tun = collect(id_tun)

    paired = {}
    for node in sorted(set(edge) & set(tun)):
        paired[node] = {"edge": edge[node], "tunnel": tun[node],
                        "tax": tun[node] - edge[node]}

    snap = {"host": HOST, "label": label, "at": time.strftime("%Y-%m-%d %H:%M:%S"),
            "nodes": paired}

    os.makedirs(SNAP_DIR, exist_ok=True)
    p = os.path.join(SNAP_DIR, f"{label}.json")
    with open(p, "w", encoding="utf-8") as f:
        json.dump(snap, f, indent=2, ensure_ascii=False)

    report(snap)
    print(f"\n  已保存 → {p}")
    return snap


def report(snap):
    nodes = snap["nodes"]
    if not nodes:
        print("  没有配对成功的节点，稍后重试。")
        return
    print(f"\n  {'节点':<8} {'到CF边缘':>9} {'经隧道':>9} {'隧道税':>9}")
    print("  " + "─" * 40)
    for n, v in sorted(nodes.items(), key=lambda kv: kv[1]["tax"]):
        print(f"  {n:<8} {v['edge']:>7}ms {v['tunnel']:>7}ms {v['tax']:>7}ms")
    taxes = sorted(v["tax"] for v in nodes.values())
    mid = taxes[len(taxes) // 2]
    print("  " + "─" * 40)
    print(f"  中位隧道税 {mid}ms   最差 {taxes[-1]}ms   样本 {len(taxes)} 个节点")


def compare(a, b):
    def load(lbl):
        p = os.path.join(SNAP_DIR, f"{lbl}.json")
        if not os.path.exists(p):
            sys.exit(f"找不到快照 {p} —— 先跑 --label {lbl}")
        with open(p, encoding="utf-8") as f:
            return json.load(f)

    sa, sb = load(a), load(b)
    common = sorted(set(sa["nodes"]) & set(sb["nodes"]))
    if not common:
        sys.exit("两次快照没有共同节点，无法对比。check-host 每次分配的节点会变，重跑一次试试。")

    print(f"▸ {a} ({sa['at']})  →  {b} ({sb['at']})")
    print(f"\n  {'节点':<8} {a+'隧道税':>12} {b+'隧道税':>12} {'变化':>10}")
    print("  " + "─" * 46)

    deltas = []
    for n in common:
        ta, tb = sa["nodes"][n]["tax"], sb["nodes"][n]["tax"]
        d = tb - ta
        deltas.append(d)
        mark = "↓" if d < -20 else ("↑" if d > 20 else "·")
        print(f"  {n:<8} {ta:>10}ms {tb:>10}ms {d:>+8}ms {mark}")

    deltas.sort()
    mid = deltas[len(deltas) // 2]
    improved = sum(1 for d in deltas if d < -20)
    worsened = sum(1 for d in deltas if d > 20)
    print("  " + "─" * 46)
    print(f"  中位变化 {mid:+}ms   改善 {improved} 节点 / 变差 {worsened} 节点 / 共 {len(deltas)}")
    print()
    print(f"  {verdict(sa, sb, deltas)}")


# ─────────────────────────────────────────────────────────────────────
# TODO(你来定): 什么样的改善才算「值得继续付费」？
#
# 这是业务判断不是技术判断，所以我没有替你写死。你比我清楚自己有多在意
# 延迟、有多在意那笔月费、以及朋友实际分布在哪些地区。几种合理的定法：
#
#   按绝对值   —— 中位数省下 >200ms 就留着（在意"快了多少毫秒"）
#   按相对值   —— 隧道税降低 >30% 就留着（在意"改善比例"）
#   按最差节点 —— 只看 max(deltas)，在意尾部体验，不让任何人特别惨
#   按地区加权 —— 朋友多在欧洲就给 de/nl/pl 节点更高权重，美国节点忽略
#
# 注意一个陷阱：check-host 每次分配的节点会变，样本量小的时候中位数不稳。
# 如果只有三五个共同节点，别急着下结论，多跑两轮。
#
# 参数: sa/sb 是两次快照 dict，deltas 是已排序的每节点变化量(ms，负数为改善)。
# 返回: 一行给人看的结论字符串。
def verdict(sa, sb, deltas):
    return "（判定标准待填 —— 见 argo-ab-test.py 里的 verdict()）"
# ─────────────────────────────────────────────────────────────────────


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description="Argo Smart Routing A/B 对比")
    ap.add_argument("--label", help="采集一次快照并存为该名字 (如 before / after)")
    ap.add_argument("--compare", nargs=2, metavar=("A", "B"), help="对比两次快照")
    args = ap.parse_args()

    if args.compare:
        compare(*args.compare)
    elif args.label:
        measure(args.label)
    else:
        ap.print_help()
