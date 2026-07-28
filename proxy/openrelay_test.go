package proxy

import (
	"strings"
	"testing"
)

// TestOpenRelayConfigurationIsRejected.
//
// AllowUnauthenticated's own comment says it is "only meaningful when the
// listener is not reachable from outside the host" -- and that precondition
// lived solely in the comment while isLoopbackHost sat with zero callers. The
// combination it allowed is the worst state this program can be in: every
// interface bound, no key required, live subscription credentials behind it.
// One field reached it, and `slimproxy check` said "配置 OK".
func TestOpenRelayConfigurationIsRejected(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "192.168.1.10"} {
		t.Run("host="+host, func(t *testing.T) {
			c := Config{Port: 8317, AllowUnauthenticated: true, Host: host}
			err := c.Validate()
			if err == nil {
				t.Fatal("绑定外部可达地址 + 免认证的组合应被拒绝")
			}
			if !strings.Contains(err.Error(), "allow-unauthenticated") {
				t.Errorf("错误信息应点名是哪个设置: %v", err)
			}
		})
	}
}

// TestLoopbackWithoutKeysIsStillAllowed: the escape hatch has to keep working
// for its actual purpose, or the check above just breaks a legitimate setup.
func TestLoopbackWithoutKeysIsStillAllowed(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		c := Config{Port: 8317, AllowUnauthenticated: true, Host: host}
		if err := c.Validate(); err != nil {
			t.Errorf("host %q 只监听本机，应当允许免认证: %v", host, err)
		}
	}
}

// TestKeysMakeTheHostIrrelevant: with keys configured, binding every interface
// is an ordinary choice -- that is how the tunnel reaches it.
func TestKeysMakeTheHostIrrelevant(t *testing.T) {
	c := Config{Port: 8317, Host: "", APIKeys: []string{"k-real-not-placeholder-5150"}}
	if err := c.Validate(); err != nil {
		t.Errorf("配置了 key 时绑定所有网卡是正常用法: %v", err)
	}
}
