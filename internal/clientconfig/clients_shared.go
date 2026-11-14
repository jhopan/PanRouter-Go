package clientconfig

import (
	"bytes"
	"strconv"
	"strings"
)

// Helpers the dashboard-only client adapters share. Everything here is about
// what the upstream route writes into a file, not about how a file is patched.

// placeholderKey is the value the old upstream routes wrote when no key was
// selected. src/app/api/cli-tools/resolveApiKey.js refuses it by name because it
// is not a credential: a config holding it authenticates with a 401. Writing it
// back would recreate exactly that bug.
const placeholderKey = "sk_PanRouter"

// cliAPIKey is the credential an adapter writes. Empty stays empty: the upstream
// resolver would fall back to the dashboard's first active key, which needs the
// database this package deliberately does not reach for.
func cliAPIKey(key string) string {
	k := strings.TrimSpace(key)
	if k == placeholderKey {
		return ""
	}
	return k
}

// cliModels is the model list a client that stores an array can write. A caller
// that sends only Model gets a one-element list; duplicates and blanks are
// dropped so an idempotent re-apply cannot grow the array.
func cliModels(opts Options) []string {
	out := make([]string, 0, len(opts.Models)+1)
	seen := make(map[string]struct{}, len(opts.Models)+1)
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" {
			return
		}
		if _, dup := seen[m]; dup {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	for _, m := range opts.Models {
		add(m)
	}
	add(opts.Model)
	return out
}

// lastModelSegment is what openclaw shows for a model: the part after the last
// slash, which is the model's own name rather than its routing prefix.
func lastModelSegment(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 && i < len(model)-1 {
		return model[i+1:]
	}
	return model
}

// removeFileWhenEmpty deletes a file the adapter has just edited down to
// nothing. The judgement needs the bytes on disk after the edit, so it runs
// after the write, like omp's reset.
func removeFileWhenEmpty(file TargetFile) (bool, error) {
	b, err := readClientFile(file)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(b)) > 0 {
		return false, nil
	}
	return removeFileIfExists(file)
}

// deleteEdits builds one Delete edit per key path.
func deleteEdits(paths ...[]string) []Edit {
	out := make([]Edit, 0, len(paths))
	for _, p := range paths {
		out = append(out, Edit{KeyPath: p, Delete: true})
	}
	return out
}

// numberLiteral renders a positive integer as the raw scalar a TOML or JSON
// value needs.
func numberLiteral(n int) string { return strconv.Itoa(n) }

// appendPath returns base with key appended, without sharing the caller's
// backing array.
func appendPath(base []string, key string) []string {
	out := make([]string, 0, len(base)+1)
	out = append(out, base...)
	return append(out, key)
}
