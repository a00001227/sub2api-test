// Package providerwebhook implements the outbound webhook gateway that
// notifies the Provider Portal of AI-account events (Phase 21E-6C-2D-1).
//
// CanonicalJSON is a BYTE-EXACT port of the Portal's canonical JSON
// (apps/provider-portal-api/src/common/canonical-json.ts) so that a
// signature produced here verifies under Portal's Sub2apiHmacGuard.
// Any divergence breaks HMAC — the golden test locks the two together.
package providerwebhook

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// CanonicalJSON serializes value deterministically:
//   - null / non-object → JSON.stringify(value ?? null)
//   - array → keep order, canonicalize each element
//   - object → drop keys whose value is undefined (Go: omitted; we drop
//     nil map entries only when explicitly modeled), sort keys lexically
//     by UTF-16-ish code-unit order (JS string comparison), emit
//     JSON.stringify(key):CanonicalJSON(value)
//
// Input must be built from Go values that map cleanly to JSON: map[string]any,
// []any, string, float64/int, bool, nil. This matches how the sender
// constructs event bodies (all string/number/bool/nested-map).
func CanonicalJSON(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return canonicalObject(v)
	case []any:
		return canonicalArray(v)
	case string:
		return jsStringifyString(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	default:
		return canonicalNumber(v)
	}
}

func canonicalObject(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k, val := range m {
		// Portal filters entries whose value is `undefined`. In Go we model
		// "absent" as the key simply not being in the map, so there is
		// nothing to filter here; explicit nil means JSON null (kept), which
		// matches Portal keeping null-valued keys.
		_ = val
		keys = append(keys, k)
	}
	// JS default sort compares by UTF-16 code units; for the ASCII keys used
	// in event payloads this equals byte-wise comparison.
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsStringifyString(k))
		b.WriteByte(':')
		b.WriteString(CanonicalJSON(m[k]))
	}
	b.WriteByte('}')
	return b.String()
}

func canonicalArray(a []any) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(CanonicalJSON(v))
	}
	b.WriteByte(']')
	return b.String()
}

// canonicalNumber renders integer-typed numbers without a decimal point,
// matching JSON.stringify for the integer values used in payloads
// (sub2api_account_id, proxy_id).
func canonicalNumber(v any) string {
	switch n := v.(type) {
	case int:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		// Integers-as-float render without ".0" to match JSON.stringify.
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'g', -1, 64)
	default:
		// Fallback: let encoding/json handle it (should not happen for the
		// constrained event payloads).
		bs, _ := json.Marshal(v)
		return string(bs)
	}
}

// jsStringifyString mirrors JavaScript JSON.stringify(string) EXACTLY.
// We cannot use json.Marshal directly: Go's encoder HTML-escapes <, >, &
// as </>/& by default, which JS does NOT — that would break
// byte-parity with Portal's canonical JSON. Use a bytes.Buffer + Encoder
// with SetEscapeHTML(false), which then matches JS for all payload strings.
func jsStringifyString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	// Encoder appends a trailing newline; strip it.
	return strings.TrimRight(buf.String(), "\n")
}
