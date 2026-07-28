package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/momo/slimproxy/proxy"
)

// cmdTest sends one real request through the running proxy.
//
// Everything `doctor` checks is a proxy for this question -- the port is bound,
// the credentials parse, DNS resolves, the upstream answers a TCP connect --
// and none of them establishes that a request actually completes. The
// translation layer, the inbound key, the credential's validity and the
// upstream's willingness all sit past the last thing doctor can see.
//
// Deliberately a separate command rather than a check inside doctor: it spends
// the operator's subscription quota. Something that costs money must be typed,
// not triggered by a diagnostic someone runs on a timer.
func cmdTest(cx *cliContext, args []string) error {
	cmd := lookup("test")
	fs := newFlagSet(cx, cmd)
	bindConfigOnly(fs, cx)
	model := fs.String("model", "", "要请求的模型；留空则取 /v1/models 的第一个")
	prompt := fs.String("prompt", "Say OK.", "发送的提示词")
	timeout := fs.Int("timeout", 60, "整体超时（秒）")
	if err := cx.parse(fs, cmd, args); err != nil {
		return skipHandled(err)
	}

	cfg, err := cx.loadConfigFor()
	if err != nil {
		return err
	}
	key := ""
	if len(cfg.APIKeys) > 0 {
		key = cfg.APIKeys[0]
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(*timeout)*time.Second)
	defer cancel()

	base := "http://" + reachableAddr(cfg)
	client := &http.Client{}

	fmt.Fprintf(cx.stdout, "目标 %s\n", base)

	chosen := *model
	if chosen == "" {
		chosen, err = firstModel(ctx, client, base, key)
		if err != nil {
			return err
		}
		fmt.Fprintf(cx.stdout, "模型 %s（未指定，取自 /v1/models 的第一个）\n", chosen)
	} else {
		fmt.Fprintf(cx.stdout, "模型 %s\n", chosen)
	}
	fmt.Fprintf(cx.stdout, "这会向上游发出一个真实请求，消耗订阅配额。\n\n")

	started := time.Now()
	body, status, err := postChat(ctx, client, base, key, chosen, *prompt)
	took := time.Since(started)

	if err != nil {
		return describeTransportError(err, base)
	}

	switch {
	case status == http.StatusOK:
		return reportSuccess(cx, body, took)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("认证失败（401）。配置里的 api-keys 与正在运行的实例不一致——"+
			"该实例可能是用另一份配置启动的。用 \"slimproxy status\" 确认端口 %s 上跑的是什么",
			cfg.Addr())
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("上游限流（429），耗时 %s。链路本身是通的：请求到达了上游并被拒绝。"+
			"等待冷却，或添加更多凭据", shortSeconds(took))
	case status >= 500:
		return fmt.Errorf("上游返回 %d，耗时 %s。运行 \"slimproxy doctor\" 检查上游可达性与 DNS 劫持\n响应: %s",
			status, shortSeconds(took), truncateBody(body))
	default:
		return fmt.Errorf("返回 %d，耗时 %s\n响应: %s", status, shortSeconds(took), truncateBody(body))
	}
}

// reachableAddr turns the configured listen address into one that can be
// dialled.
//
// A config binding 0.0.0.0 is not an address to connect to; the loopback is
// where the listener is actually reachable from here.
func reachableAddr(cfg *proxy.Config) string {
	host := cfg.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprint(cfg.Port))
}

// firstModel asks the proxy what it can serve.
//
// Better than hardcoding a model name, which would go stale every time a
// provider ships a build, and would send the operator chasing a 404 that has
// nothing to do with their deployment.
func firstModel(ctx context.Context, c *http.Client, base, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", describeTransportError(err, base)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Naming -model here would be misleading: the request never got past
		// authentication, so no model would help.
		return "", errors.New("认证失败（401）。这份配置里的 api-keys 与正在运行的实例不一致——" +
			"该实例可能是用另一份配置启动的。用 \"slimproxy status\" 确认端口上跑的是什么")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("无法获取模型列表（%d）：%s\n用 -model 手动指定一个",
			resp.StatusCode, truncateBody(raw))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("模型列表无法解析：%w\n用 -model 手动指定一个", err)
	}
	if len(parsed.Data) == 0 {
		return "", errors.New("代理没有报告任何可用模型；" +
			"通常意味着凭据池为空或全部失效，运行 \"slimproxy auth list\" 查看")
	}
	return parsed.Data[0].ID, nil
}

func postChat(ctx context.Context, c *http.Client, base, key, model, prompt string) ([]byte, int, error) {
	payload, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		// Small on purpose: this proves the path works, and every token is the
		// operator's own quota.
		"max_tokens": 16,
	})
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, err
}

// reportSuccess distinguishes a completion from an empty 200.
//
// A 200 carrying no content is the shape an upstream policy refusal used to
// take before proxy/refusal.go rewrote it -- so "status was 200" is not on its
// own evidence that the path works.
func reportSuccess(cx *cliContext, body []byte, took time.Duration) error {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("返回 200 但响应无法解析：%w\n响应: %s", err, truncateBody(body))
	}
	if len(parsed.Choices) == 0 {
		return fmt.Errorf("返回 200 但没有任何 choice，耗时 %s。\n响应: %s",
			shortSeconds(took), truncateBody(body))
	}

	first := parsed.Choices[0]
	content := strings.TrimSpace(first.Message.Content)

	fmt.Fprintf(cx.stdout, "成功，耗时 %s\n", shortSeconds(took))
	fmt.Fprintf(cx.stdout, "  finish_reason  %s\n", orDash(first.FinishReason))
	if parsed.Usage.TotalTokens > 0 {
		fmt.Fprintf(cx.stdout, "  tokens         %d\n", parsed.Usage.TotalTokens)
	}
	fmt.Fprintf(cx.stdout, "  回复           %s\n", orDash(truncateOneLine(content, 60)))

	if content == "" {
		fmt.Fprintf(cx.stdout, "\n注意：状态码是 200 但回复为空。"+
			"若 finish_reason 是 content_filter，说明上游按策略拒绝了这次请求——"+
			"链路是通的。\n")
	}
	return nil
}

// describeTransportError explains a failure to reach the proxy at all, which
// almost always means it is not running.
func describeTransportError(err error, base string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("请求超时。代理接受了连接但没有在期限内完成——"+
			"运行 \"slimproxy doctor\" 检查上游可达性，或用 -timeout 延长")
	}
	var netErr net.Error
	if errors.As(err, &netErr) || strings.Contains(err.Error(), "connect") {
		return fmt.Errorf("无法连接 %s：%w\n代理似乎没有在运行。先启动它（另开一个终端运行 slimproxy），"+
			"或用 \"slimproxy status\" 确认", base, err)
	}
	return fmt.Errorf("请求失败: %w", err)
}

func shortSeconds(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func truncateBody(b []byte) string {
	return truncateOneLine(string(b), 300)
}

// truncateOneLine flattens and shortens a response for a one-line report.
func truncateOneLine(s string, max int) string {
	s = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s))
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
