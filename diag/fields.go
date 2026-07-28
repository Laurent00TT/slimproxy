package diag

import (
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/momo/slimproxy/proxy"
)

// UnknownFields reports top-level config keys this binary does not recognise.
//
// Derived by reflecting over proxy.Config's yaml tags rather than by parsing
// the decoder's error text. The decoder does report unknown fields when
// KnownFields is set, but only as prose ("field X not found in type ..."), and
// scraping that would break silently the day the message is reworded -- which
// is exactly the class of failure this check exists to catch.
func UnknownFields(body []byte) ([]string, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	known := knownConfigKeys()
	var unknown []string
	for k := range raw {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// knownConfigKeys returns every yaml key proxy.Config accepts.
func knownConfigKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(proxy.Config{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out[name] = true
		}
	}
	return out
}
