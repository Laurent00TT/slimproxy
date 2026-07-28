package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// The supervisor's job is to make one process out of two things that can each
// fail independently, and to exit on the outcome that explains what happened.
// Every case below is one the operator meets on a bad day and never on a good
// one, which is why none of them is observable by running the dashboard.

func newSupervisorCtx() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// TestProxyFailureTakesDownTheDashboard.
//
// The proxy failing to bind is the most common bad start there is. When it
// happens the dashboard must not sit there rendering an empty panel: the
// reason is in a log file the alternate screen is covering, and the operator
// has no way to see it until the screen is released.
func TestProxyFailureTakesDownTheDashboard(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	bindErr := errors.New("listen tcp 127.0.0.1:8317: bind: address already in use")

	uiSawCancel := make(chan struct{})
	err := supervise(ctx, cancel,
		func(context.Context) error { return bindErr },
		// The UI exits only on cancellation -- which is the real behaviour, and
		// the reason for the escape hatch. supervise blocks here, so a version
		// that forgets to cancel deadlocks rather than failing, and a deadlock
		// costs the whole `go test` timeout to diagnose. The deadline turns
		// that into an assertion.
		func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				close(uiSawCancel)
			case <-time.After(2 * time.Second):
			}
			return nil
		},
		time.Second, &bytes.Buffer{})

	select {
	case <-uiSawCancel:
	default:
		t.Error("代理失败后应取消面板；面板等到超时也没被取消")
	}
	if !errors.Is(err, bindErr) {
		t.Errorf("返回 %v，应为代理的绑定错误", err)
	}
}

// TestQuittingTheDashboardStopsTheProxy pins the other direction. Pressing q
// on a foreground process is understood to stop it, not to leave a listener
// running with no interface.
func TestQuittingTheDashboardStopsTheProxy(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	proxyStopped := make(chan struct{})
	err := supervise(ctx, cancel,
		func(ctx context.Context) error {
			<-ctx.Done()
			close(proxyStopped)
			return ctx.Err() // what Service.Run returns on a planned shutdown
		},
		func(context.Context) error { return nil }, // operator pressed q
		2*time.Second, &bytes.Buffer{})

	select {
	case <-proxyStopped:
	case <-time.After(time.Second):
		t.Fatal("面板退出后代理未被停止")
	}
	if err != nil {
		t.Errorf("正常退出应返回 nil，实际 %v", err)
	}
}

// TestPlannedShutdownIsNotAFailure.
//
// Service.Run returns ctx.Err() unconditionally, so every clean stop arrives
// as context.Canceled. Treating that as an error would make every quit exit
// non-zero -- and under a service manager, every intentional stop look like a
// crash worth restarting.
func TestPlannedShutdownIsNotAFailure(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	err := supervise(ctx, cancel,
		func(ctx context.Context) error { <-ctx.Done(); return context.Canceled },
		func(context.Context) error { return nil },
		time.Second, &bytes.Buffer{})
	if err != nil {
		t.Errorf("context.Canceled 不应作为失败上报，实际 %v", err)
	}
}

// TestDashboardErrorSurvivesACleanProxyStop: when only the UI failed, that is
// the failure to report.
func TestDashboardErrorSurvivesACleanProxyStop(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	uiErr := errors.New("终端不支持所需的能力")
	err := supervise(ctx, cancel,
		func(ctx context.Context) error { <-ctx.Done(); return context.Canceled },
		func(context.Context) error { return uiErr },
		time.Second, &bytes.Buffer{})
	if !errors.Is(err, uiErr) {
		t.Errorf("返回 %v，应为面板的错误", err)
	}
}

// TestProxyErrorOutranksDashboardError.
//
// When both fail, the proxy's reason is the one that explains the situation:
// the dashboard's failure is usually a consequence of it.
func TestProxyErrorOutranksDashboardError(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	proxyErr := errors.New("绑定端口失败")
	err := supervise(ctx, cancel,
		func(context.Context) error { return proxyErr },
		func(ctx context.Context) error { <-ctx.Done(); return errors.New("面板被取消") },
		time.Second, &bytes.Buffer{})
	if !errors.Is(err, proxyErr) {
		t.Errorf("返回 %v，应优先上报代理的错误", err)
	}
}

// TestHungProxyIsReportedNotWaitedOnForever.
//
// CLIProxyAPI fixes its shutdown deadline at startup rather than at signal
// time, so a long-lived process does not get a graceful drain and can fail to
// stop. Blocking forever would hang the exit; exiting silently would leave the
// next start failing on a port conflict with no explanation anywhere.
func TestHungProxyIsReportedNotWaitedOnForever(t *testing.T) {
	ctx, cancel := newSupervisorCtx()
	defer cancel()

	release := make(chan struct{})
	defer close(release)

	var warn bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, cancel,
			func(context.Context) error { <-release; return nil }, // never stops
			func(context.Context) error { return nil },
			150*time.Millisecond, &warn)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("宽限期用尽后应正常返回，实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("代理不退出时 supervise 阻塞了")
	}

	if !strings.Contains(warn.String(), "未停止") {
		t.Errorf("应向 stderr 说明代理未停止，实际输出 %q", warn.String())
	}
}

// TestEnsureCanBindNamesTheLikelyCause.
//
// The dashboard must not take the screen when the port is already held. The
// failure it prevents is specific: CLIProxyAPI reports a bind error only after
// printing "API server started successfully" and loading every credential, so
// in panel mode the sequence renders as a dashboard that appears and vanishes
// a second later -- a crash, as far as the operator can tell, with the actual
// error scrolling past after the alternate screen has already gone.
func TestEnsureCanBindNamesTheLikelyCause(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法建立测试监听: %v", err)
	}
	defer func() { _ = ln.Close() }()

	err = ensureCanBind(ln.Addr().String())
	if err == nil {
		t.Fatal("端口已被占用时应报错，否则面板会先占屏再消失")
	}
	if !strings.Contains(err.Error(), "slimproxy status") {
		t.Errorf("错误信息应告诉操作者怎么查: %v", err)
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("错误信息应点名是哪个地址: %v", err)
	}
}

// TestEnsureCanBindAllowsAFreePort pins that the check does not itself hold the
// port it just tested -- the proxy binds it moments later.
func TestEnsureCanBindAllowsAFreePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法建立测试监听: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if err := ensureCanBind(addr); err != nil {
		t.Fatalf("空闲端口应通过: %v", err)
	}
	// Bindable again immediately: the check must release what it probed.
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("探测之后端口仍被占用，说明检查没有释放它: %v", err)
	}
	_ = again.Close()
}
