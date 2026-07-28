package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEffectiveConfigNameIsPerInstance.
//
// The name was a constant while the state directory defaults to the working
// directory, so a second instance started in the same folder overwrote the file
// the first one serves from -- and the upstream watcher reloads that file. A
// second instance carrying `allow-unauthenticated: true` could therefore turn a
// running, publicly exposed proxy into an open relay without touching it.
func TestEffectiveConfigNameIsPerInstance(t *testing.T) {
	a, b := EffectiveConfigName(8317), EffectiveConfigName(8318)
	if a == b {
		t.Fatalf("两个端口生成了同一个文件名 %q：同目录下的实例会互相覆盖", a)
	}
	for _, n := range []string{a, b} {
		if !strings.HasSuffix(n, ".yaml") || strings.ContainsAny(n, `/\`) {
			t.Errorf("文件名 %q 不合法", n)
		}
	}
}

// TestAuthGuardStopsWhenKeysVanish.
//
// Validate() runs once, in Build. The upstream reload path does not re-validate,
// and an empty api-keys list unregisters the only access provider -- after which
// every request is admitted, on a process holding live subscription
// credentials. The only upstream trace is a debug line reading
// "api-keys count: 1 -> 0".
func TestAuthGuardStopsWhenKeysVanish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effective.yaml")
	if err := os.WriteFile(path, []byte("api-keys:\n  - \"k\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{ConfigPath: path, requireKeys: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go rt.guardInboundAuth(ctx, func() { close(stopped) })

	// Still authenticated: must not fire.
	select {
	case <-stopped:
		t.Fatal("配置未被削弱时守卫就停止了服务")
	case <-time.After(authGuardInterval + 500*time.Millisecond):
	}

	// Someone empties the key list -- by hand, or by another instance
	// materialising over this file.
	if err := os.WriteFile(path, []byte("api-keys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-stopped:
	case <-time.After(3 * authGuardInterval):
		t.Error("入站认证被清空后服务没有停止：代理会继续放行每一个请求")
	}
}

// TestAuthGuardStandsDownWhenUnauthenticatedWasChosen.
//
// The guard defends an invariant that existed. An operator who deliberately
// configured allow-unauthenticated never had one, and stopping their proxy in a
// loop would be the tool overriding an explicit decision.
func TestAuthGuardStandsDownWhenUnauthenticatedWasChosen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effective.yaml")
	if err := os.WriteFile(path, []byte("api-keys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{ConfigPath: path, requireKeys: false}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go rt.guardInboundAuth(ctx, func() { close(stopped) })

	select {
	case <-stopped:
		t.Error("显式选择了 allow-unauthenticated 的实例不应被守卫停掉")
	case <-time.After(authGuardInterval + 500*time.Millisecond):
	}
}

// TestAuthGuardToleratesAnUnreadableFile: a transient read failure is not
// evidence that authentication was removed, and stopping a working proxy over
// one would be its own outage.
func TestAuthGuardToleratesAnUnreadableFile(t *testing.T) {
	rt := &Runtime{ConfigPath: filepath.Join(t.TempDir(), "absent.yaml"), requireKeys: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go rt.guardInboundAuth(ctx, func() { close(stopped) })

	select {
	case <-stopped:
		t.Error("配置文件读不到时不应停止服务")
	case <-time.After(authGuardInterval + 500*time.Millisecond):
	}
}
