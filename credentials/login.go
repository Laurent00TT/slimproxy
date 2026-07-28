package credentials

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxyconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// Provider describes one upstream slimproxy can hold credentials for.
type Provider struct {
	// Name is the identifier `auth add` takes.
	Name string
	// Summary is a one-line description for help output.
	Summary string
	// Interactive says the flow needs the operator to act somewhere else --
	// a browser page or a device-code prompt -- rather than completing on its
	// own.
	Interactive bool
}

// providers is the set this build can authenticate.
//
// Derived from the authenticators registered below rather than hand-listed, so
// the two cannot drift: a provider that appears here but has no authenticator
// would produce a command that always fails.
var providers = []Provider{
	{Name: "claude", Summary: "Anthropic Claude（浏览器授权）", Interactive: true},
	{Name: "codex", Summary: "OpenAI Codex（浏览器授权）", Interactive: true},
	{Name: "antigravity", Summary: "Antigravity（浏览器授权）", Interactive: true},
	{Name: "kimi", Summary: "Kimi（设备码）", Interactive: true},
	{Name: "xai", Summary: "xAI（设备码）", Interactive: true},
}

// Providers returns the supported providers, sorted by name.
func Providers() []Provider {
	out := append([]Provider(nil), providers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProviderNames returns just the names, for error messages.
func ProviderNames() []string {
	out := make([]string, 0, len(providers))
	for _, p := range Providers() {
		out = append(out, p.Name)
	}
	return out
}

// KnownProvider reports whether name is supported.
func KnownProvider(name string) bool {
	for _, p := range providers {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// newManager builds an auth manager over the file store.
//
// Every authenticator is registered regardless of which one is about to be
// used: registration is cheap, and a manager that knows only one provider would
// report "not registered" for a typo instead of the provider list.
func newManager() *sdkauth.Manager {
	return sdkauth.NewManager(
		sdkauth.NewFileTokenStore(),
		sdkauth.NewClaudeAuthenticator(),
		sdkauth.NewCodexAuthenticator(),
		sdkauth.NewAntigravityAuthenticator(),
		sdkauth.NewKimiAuthenticator(),
		sdkauth.NewXAIAuthenticator(),
	)
}

// LoginRequest is one authentication attempt.
type LoginRequest struct {
	// Provider is which upstream to authenticate against.
	Provider string
	// AuthDir is the resolved directory the credential is written to. It must
	// be the same directory the proxy loads from -- see proxy.Config.ResolveAuthDir.
	AuthDir string
	// ProxyURL routes the OAuth exchange through the same upstream proxy the
	// server path uses.
	//
	// Required for correctness, not convenience: when the config sets one, the
	// serving path honours it and a login that does not would reach the
	// provider directly. On a machine that needs the proxy, `serve` works while
	// `auth add` times out -- with nothing to suggest the setting was ignored.
	ProxyURL string
	// NoBrowser prints the authorisation URL instead of opening it. Necessary
	// on a headless machine, and useful when the default browser is not the one
	// signed in to the account being added.
	NoBrowser bool
	// CallbackPort overrides the local port the OAuth redirect returns to.
	// Zero lets the provider's authenticator choose.
	CallbackPort int
	// Prompt is how the flow asks the operator for something -- a pasted code,
	// a confirmation. Required: an authenticator that needs input and has no
	// way to ask would block forever.
	Prompt func(prompt string) (string, error)
}

// Login runs a provider's OAuth flow and stores the resulting credential.
//
// Returns the path the credential was written to, which is what the operator
// needs in order to confirm it landed where the proxy will look.
func Login(ctx context.Context, req LoginRequest) (string, error) {
	if !KnownProvider(req.Provider) {
		return "", fmt.Errorf("不支持的 provider %q（可用: %s）",
			req.Provider, strings.Join(ProviderNames(), ", "))
	}
	if req.AuthDir == "" {
		return "", fmt.Errorf("未指定凭据目录")
	}
	if req.Prompt == nil {
		// The flows can require a pasted code. Without a prompt the SDK has no
		// way to ask, and the command would appear to hang.
		return "", fmt.Errorf("内部错误：登录流程缺少交互回调")
	}

	mgr := newManager()
	cfg := loginConfig(req)
	opts := &sdkauth.LoginOptions{
		NoBrowser:    req.NoBrowser,
		CallbackPort: req.CallbackPort,
		Prompt:       req.Prompt,
	}

	_, savedPath, err := mgr.Login(ctx, strings.ToLower(req.Provider), cfg, opts)
	if err != nil {
		return "", err
	}
	if savedPath == "" {
		// The SDK returns an empty path when it has no store, which cannot
		// happen here -- but reporting success without a file would leave the
		// operator believing a credential exists that does not.
		return "", fmt.Errorf("登录成功但凭据未被写入磁盘；请检查 %s 的权限", req.AuthDir)
	}
	return savedPath, nil
}

// loginConfig builds the upstream configuration one login runs against.
//
// Split out so what reaches the SDK can be asserted without starting an OAuth
// flow. The proxy-url in particular has no visible effect until it is missing:
// the serving path honours the setting, so a login that dropped it would reach
// the provider directly and time out on exactly the machines that need it.
func loginConfig(req LoginRequest) *cliproxyconfig.Config {
	cfg := &cliproxyconfig.Config{}
	cfg.AuthDir = req.AuthDir
	cfg.ProxyURL = req.ProxyURL
	return cfg
}
