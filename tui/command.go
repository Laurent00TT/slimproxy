package tui

import (
	"fmt"
	"strings"

	"github.com/Laurent00TT/slimproxy/tunnel"
)

// Command is one slash command.
type Command struct {
	// Name is what the operator types, without the leading slash. Multi-word
	// names ("tunnel down") are matched whole, so typing "tunnel" narrows to
	// the tunnel family rather than matching a single command called tunnel.
	Name string
	// Summary is the one-line description in the completion list.
	Summary string
	// Args describes the arguments, for the hint shown when a command needs
	// them. Empty means the command takes none.
	Args string
	// Danger marks a command whose effect reaches outside this process and
	// cannot be undone by pressing a key. These ask before acting.
	Danger bool
	// Consequence states what will happen, evaluated against live state at the
	// moment the row is selected.
	//
	// Present in a design that has no dedicated confirmation panel because the
	// warning matters more than the panel: "停止隧道" says what the command
	// does, "当前 2 连接 · 停止后外网立即不可达" says what it costs. The second
	// one is the sentence that stops a mistake.
	Consequence func(m *Model) string
	// Run performs the command. It returns a tea.Cmd rather than acting
	// inline, because everything here touches the network or the filesystem
	// and the render loop must not block on it.
	Run func(m *Model, args []string) actionSpec
}

// commandSet is the registry, in the order the completion list shows them.
//
// Ordered by how often they are wanted, not alphabetically: the tunnel
// switches are the reason this exists.
//
// monitor leads anyway, ahead of them. It is the only entry that changes
// nothing outside this display, and the moment it is wanted is the moment the
// operator is looking at the wrong screen and opening this list to find the way
// back -- which makes first the only place it can be.
func commandSet() []*Command {
	return []*Command{
		{
			Name:    "monitor",
			Summary: "回到实时请求流（与 Esc 等效）",
			Run:     runMonitor,
		},
		{
			Name:    "tunnel status",
			Summary: "查询隧道三态与 Cloudflare 侧连接数",
			Run:     runTunnelStatus,
		},
		{
			Name:    "tunnel up",
			Summary: "启动 cloudflared 并等待注册连接",
			Consequence: func(m *Model) string {
				if m.tunnel.known && m.tunnel.st.State == tunnel.Running && m.tunnel.st.Managed {
					return "已在运行，此命令会报错而不会重复启动"
				}
				if m.tunnel.known && m.tunnel.hostname != "" {
					return "成功后 " + m.tunnel.hostname + " 将对公网可达"
				}
				return "最多等待 15 秒确认连接建立"
			},
			Run: runTunnelUp,
		},
		{
			Name:    "tunnel down",
			Summary: "停止本进程启动的 cloudflared",
			Danger:  true,
			Consequence: func(m *Model) string {
				if !m.tunnel.known {
					return "隧道状态尚未确认"
				}
				switch {
				case m.tunnel.st.Managed && m.tunnel.connKnown && m.tunnel.conns > 0:
					return fmt.Sprintf("当前 %d 连接 · 停止后 %s 立即不可达",
						m.tunnel.conns, hostnameOrPhrase(m.tunnel.hostname))
				case m.tunnel.st.Managed:
					return "停止后 " + hostnameOrPhrase(m.tunnel.hostname) + " 立即不可达"
				default:
					return "没有由本进程启动的隧道，此命令不会终止任何进程"
				}
			},
			Run: runTunnelDown,
		},
		{
			Name:    "auth list",
			Summary: "列出凭据池及各自状态",
			Run:     runAuthList,
		},
		{
			Name:    "auth rm",
			Summary: "删除一个凭据",
			Args:    "<名称或邮箱>",
			Danger:  true,
			Consequence: func(m *Model) string {
				if !m.creds.known {
					return "凭据池尚未读取"
				}
				switch n := m.creds.recoverable; n {
				case 0:
					// Was reported as "仅剩 1 个" -- a count of a credential
					// that does not exist, because the message hardcoded the
					// number the branch condition allowed to be zero.
					return "池中已无可恢复凭据"
				case 1:
					return "池中仅剩 1 个可恢复凭据，删除后每个请求都会失败"
				default:
					return fmt.Sprintf("池中共 %d 个可恢复凭据", n)
				}
			},
			Run: runAuthRemove,
		},
		{
			Name:    "auth add",
			Summary: "添加凭据（需退出后在命令行完成）",
			Args:    "<provider>",
			Run:     runAuthAdd,
		},
		{
			Name:    "doctor",
			Summary: "运行完整诊断（可能耗时数十秒）",
			Run:     runDoctor,
		},
		{
			Name:    "routes",
			Summary: "列出本构建支持的翻译路由",
			Run:     runRoutes,
		},
		{
			Name:    "quit",
			Summary: "停止代理并退出",
			// Not Danger, deliberately. The q key means the same thing and
			// cannot reasonably prompt, so marking only the typed spelling
			// dangerous would give one action two behaviours. The guard that
			// matters -- refusing once while an action is in flight -- lives in
			// requestQuit and covers both.
			Consequence: func(m *Model) string {
				if m.tunnel.known && m.tunnel.st.Managed {
					return "隧道是后台进程，退出后仍会继续运行（用 tunnel down 停止）"
				}
				return "本地监听立即停止"
			},
			Run: runQuit,
		},
	}
}

// hostnameOrPhrase names the tunnel's public hostname, falling back to a phrase
// that reads correctly in "停止后 X 立即不可达".
//
// Not called orDash: it never returns a dash, and a function of that name in
// this package that does return one would be a genuine trap.
func hostnameOrPhrase(s string) string {
	if strings.TrimSpace(s) == "" {
		return "隧道主机名"
	}
	return s
}

// match returns the commands the current input selects, and the arguments the
// operator typed beyond a fully-matched command name.
//
// Two passes, prefix before substring. Prefix alone would make "down"
// unmatchable without typing "tunnel" first; substring alone would rank
// "tunnel status" and "auth list" together for the query "t". Prefix results
// win outright when there are any, so the common case stays predictable.
func match(cmds []*Command, input string) (hits []*Command, args []string) {
	q := strings.TrimSpace(strings.TrimPrefix(input, "/"))
	if q == "" {
		return cmds, nil
	}
	lower := strings.ToLower(q)

	// An exactly-named command followed by a space means the rest is
	// arguments, not a narrower query: "/auth rm claude.json" must resolve to
	// auth rm with one argument rather than matching nothing.
	for _, c := range cmds {
		p := strings.ToLower(c.Name) + " "
		if strings.HasPrefix(lower, p) {
			rest := strings.Fields(q[len(c.Name):])
			return []*Command{c}, rest
		}
	}

	for _, c := range cmds {
		if strings.HasPrefix(strings.ToLower(c.Name), lower) {
			hits = append(hits, c)
		}
	}
	if len(hits) > 0 {
		return hits, nil
	}
	for _, c := range cmds {
		if strings.Contains(strings.ToLower(c.Name), lower) {
			hits = append(hits, c)
		}
	}
	return hits, nil
}
