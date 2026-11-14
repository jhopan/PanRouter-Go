// Package envx reads this gateway's environment variables under every prefix it
// has shipped with.
//
// The product was renamed from 9router-go to PanRouter-Go, and the Go port before
// that accepted NINEROUTER_* alongside the original 9ROUTER_* names. An operator
// upgrading keeps their .env and their service unit: a read that only knew the
// new spelling would silently fall back to a default — a live-test gate that never
// opens, a route-header switch that never turns off, a peer token that never
// matches. So every read goes through here, and the new prefix wins when both are
// set.
package envx

import (
	"os"
	"strings"
)

// Prefixes in precedence order: the current name first, then the two this project
// shipped with. Never reorder without reading the package doc.
var Prefixes = []string{"PANROUTER_", "9ROUTER_", "NINEROUTER_"}

// Get returns the first non-empty value for name under any known prefix, or "".
func Get(name string) string {
	v, _ := Lookup(name)
	return v
}

// Lookup is Get plus whether any prefix was set (an empty value counts as set only
// when the variable exists with non-blank content).
func Lookup(name string) (string, bool) {
	for _, p := range Prefixes {
		if v := strings.TrimSpace(os.Getenv(p + name)); v != "" {
			return v, true
		}
	}
	return "", false
}

// Names returns every spelling of name, new prefix first, for callers that build
// their own lookup list (they keep control of parsing and diagnostics).
func Names(name string) []string {
	out := make([]string, 0, len(Prefixes))
	for _, p := range Prefixes {
		out = append(out, p+name)
	}
	return out
}
