package tunnel

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// Config is the part of a cloudflared configuration this package reads.
type Config struct {
	// Tunnel is the tunnel UUID or name. `cloudflared tunnel info` accepts
	// either, so it is used as-is.
	Tunnel string `yaml:"tunnel"`
	// Hostnames are the ingress hostnames that actually route somewhere, in
	// rule order.
	//
	// A list, not a first-match: one tunnel serves as many hostnames as it has
	// ingress rules, and reporting only the first made every extra domain
	// invisible -- an operator who had just added one concluded it had not
	// taken effect.
	Hostnames []string
	// Path is where the config was found.
	Path string
}

// ReadConfig reads the cloudflared configuration from its conventional
// location.
//
// Three outcomes, deliberately not two:
//
//	found=false, err=nil    no config file -- a legitimate "no tunnel" state
//	found=false, err!=nil   a config exists but could not be understood
//	found=true,  err=nil    usable
//
// The middle case is why this does not return a bare boolean. Collapsing
// "unreadable" into "absent" makes a broken tunnel report as an absent one, and
// an absent tunnel is a PASS -- so a corrupt config would show a green
// diagnostic while the public hostname served 502.
func ReadConfig() (cfg Config, found bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, false, fmt.Errorf(i18n.T("无法确定用户主目录: %w", "cannot determine the home directory: %w"), err)
	}
	path := filepath.Join(home, ".cloudflared", "config.yml")

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, false, nil // genuinely not configured
		}
		return Config{}, false, fmt.Errorf(i18n.T("无法读取 %s: %w", "cannot read %s: %w"), path, err)
	}
	return parseConfig(body, path)
}

// parseConfig turns a config file's bytes into a Config. Split from ReadConfig
// so the ingress-rule decisions below are testable without a home directory.
func parseConfig(body []byte, path string) (Config, bool, error) {
	var doc struct {
		Tunnel  string `yaml:"tunnel"`
		Ingress []struct {
			Hostname string `yaml:"hostname"`
			Service  string `yaml:"service"`
		} `yaml:"ingress"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return Config{}, false, fmt.Errorf(i18n.T("%s 解析失败: %w", "parsing %s failed: %w"), path, err)
	}
	if strings.TrimSpace(doc.Tunnel) == "" {
		return Config{}, false, fmt.Errorf(i18n.T("%s 中没有 tunnel 字段", "%s has no tunnel field"), path)
	}
	// The tunnel id becomes a command-line argument to cloudflared, so a value
	// starting with "-" stops being data and becomes a flag.
	//
	// exec.Command uses no shell, so this is not shell injection -- it is
	// argument injection, which is enough: `tunnel: "--token=<attacker's>"`
	// would run a tunnel under someone else's Cloudflare account with this
	// machine's port 8317 behind it. Requires write access to the cloudflared
	// config, so it is not an active threat; it is a config value silently
	// promoted to an executable argument, which is worth closing before it is.
	if !validTunnelID(doc.Tunnel) {
		return Config{}, false, fmt.Errorf(i18n.T(
			"%s 中的 tunnel 值 %q 不是合法的隧道标识（应为 UUID 或名称，不能以 - 开头）",
			"the tunnel value %[2]q in %[1]s is not a valid tunnel identifier (expected a UUID or name, must not start with -)"),
			path, doc.Tunnel)
	}

	out := Config{Tunnel: doc.Tunnel, Path: path}
	seen := map[string]bool{}
	for _, rule := range doc.Ingress {
		// HasPrefix, not Service[:4]: a hand-written `service: ssh` is three
		// bytes and would panic on a slice bound -- and this runs before the
		// per-check recover, so it would take the whole command down.
		//
		// Only http/https URLs route to an origin. http_status:* rules exist to
		// block paths, so a hostname whose only rule is one of those is not
		// actually reachable and must not be reported as the public hostname.
		if rule.Hostname == "" {
			continue
		}
		if strings.HasPrefix(rule.Service, "http://") || strings.HasPrefix(rule.Service, "https://") {
			// Deduplicated: cloudflared's path-splitting puts one hostname on
			// several rules, and counting each rule as a domain would make the
			// top bar claim "+1" for a second domain that does not exist --
			// fabricating the very signal this list exists to make honest.
			if seen[rule.Hostname] {
				continue
			}
			seen[rule.Hostname] = true
			out.Hostnames = append(out.Hostnames, rule.Hostname)
		}
	}
	return out, true, nil
}

// JoinHostnames renders a hostname list for one-line contexts: the journal
// note, the tunnel-down preamble, a diagnostic detail.
//
// One function rather than strings.Join at every site so the separator cannot
// drift between surfaces -- and so a caller cannot accidentally render an empty
// list as an empty string where a phrase like "该隧道" was needed; the fallback
// stays the caller's decision, visibly.
func JoinHostnames(hs []string) string {
	return strings.Join(hs, "、")
}

// tunnelIDPattern accepts what cloudflared accepts as a tunnel identifier: a
// UUID, or a tunnel name. Both are alphanumeric with dashes, dots and
// underscores, and neither begins with a dash.
var tunnelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validTunnelID reports whether an id can be passed to cloudflared as an
// argument without changing what the command means.
func validTunnelID(s string) bool {
	return tunnelIDPattern.MatchString(strings.TrimSpace(s))
}
