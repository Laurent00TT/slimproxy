package main

import (
	"net"
	"testing"
)

// TestLoginProxyURL: login must make the same proxy-or-direct call the
// serving path makes, or `auth add` fails against a dead proxy port that the
// proxy itself would sidestep -- and the operator's first contact with the
// fallback feature would be it appearing not to exist.
func TestLoginProxyURL(t *testing.T) {
	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	liveURL := "http://" + live.Addr().String()

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	_ = dead.Close()

	for _, tc := range []struct {
		name     string
		set      authSettings
		wantURL  string
		wantFell bool
	}{
		{"no proxy configured", authSettings{}, "", false},
		{"proxy up, fallback on", authSettings{ProxyURL: liveURL, FallbackDirect: true}, liveURL, false},
		{"proxy down, fallback on", authSettings{ProxyURL: deadURL, FallbackDirect: true}, "", true},
		// Without the flag a dead proxy is used as configured: failing
		// loudly there is the pre-existing contract, not this feature's to
		// change.
		{"proxy down, fallback off", authSettings{ProxyURL: deadURL}, deadURL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, fell := loginProxyURL(tc.set)
			if got != tc.wantURL || fell != tc.wantFell {
				t.Errorf("loginProxyURL = (%q, %v), want (%q, %v)", got, fell, tc.wantURL, tc.wantFell)
			}
		})
	}
}
