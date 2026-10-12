// Package hintconf is the reference conformance checker for the HINT data standard v0.1.
//
// It is a test tool, not a producer or consumer of HINT data. A third party can validate
// HINT documents with any JSON Schema 2020-12 validator and an RFC 8785 implementation;
// this package adds the semantic rules of docs/standard/hint-data-v0.1.md §9 and the
// client import validation of docs/standard/exchange-v0.1.md §4.
package hintconf

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// IDPrefix is the domain-separation prefix of every derived identifier (hint-data §4).
const IDPrefix = "hint:v0.1:"

// DeriveID returns HMAC-SHA256(key, "hint:v0.1:" + domain + ("\x00" + part)...),
// truncated to 16 bytes and encoded as 32 lowercase hex characters.
func DeriveID(key []byte, domain string, parts ...string) string {
	return hex.EncodeToString(derive(key, domain, parts)[:16])
}

func derive(key []byte, domain string, parts []string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(IDPrefix + domain))
	for _, p := range parts {
		m.Write([]byte{0})
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

// TraceID pseudonymizes a W3C trace-id for export (hint-data §4.1): the first 16 bytes of
// HMAC-SHA256(K, "hint:v0.1:trace" + "\x00" + source trace-id), as 32 lowercase hex
// characters. ok is false for a source value that is not a valid W3C trace-id; the producer
// then leaves the field out and lists it in missing. The source value is never exported.
func TraceID(key []byte, sourceTrace string) (string, bool) {
	t, ok := w3c(sourceTrace, 32)
	if !ok {
		return "", false
	}
	return nonZero(key, "trace", []string{t}, 16), true
}

// SpanID pseudonymizes a W3C span-id (parent-id) of the given trace: the first 8 bytes of
// HMAC-SHA256(K, "hint:v0.1:span" + "\x00" + source trace-id + "\x00" + source span-id), as
// 16 lowercase hex characters. A parentSpanId uses the same function with the trace-id of
// its span, so it equals the exported span_id of the parent span.
func SpanID(key []byte, sourceTrace, sourceSpan string) (string, bool) {
	t, ok1 := w3c(sourceTrace, 32)
	sp, ok2 := w3c(sourceSpan, 16)
	if !ok1 || !ok2 {
		return "", false
	}
	return nonZero(key, "span", []string{t, sp}, 8), true
}

// nonZero truncates the HMAC to n bytes. W3C forbids an all-zero ID; in that case (chance
// 2^-64 for a span) the derivation repeats with the extra parts "1", "2", ... until non-zero.
func nonZero(key []byte, domain string, parts []string, n int) string {
	for i := 0; ; i++ {
		ps := parts
		if i > 0 {
			ps = append(append([]string{}, parts...), strconv.Itoa(i))
		}
		b := derive(key, domain, ps)[:n]
		for _, x := range b {
			if x != 0 {
				return hex.EncodeToString(b)
			}
		}
	}
}

// w3c normalizes a source W3C ID to lowercase hex and rejects a wrong length, a non-hex
// character or the all-zero value.
func w3c(v string, n int) (string, bool) {
	v = strings.ToLower(v)
	if len(v) != n || v == strings.Repeat("0", n) {
		return "", false
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", false
	}
	return v, true
}

// Convenience wrappers for the identifier domains of hint-data §4.
func NamespaceID(key []byte) string                    { return DeriveID(key, "namespace") }
func RepoID(key []byte, name string) string            { return DeriveID(key, "repo", name) }
func FileID(key []byte, repo, path string) string      { return DeriveID(key, "file", repo, path) }
func ContractID(key []byte, kind, ident string) string { return DeriveID(key, "contract", kind, ident) }
func SymbolID(key []byte, repo, lang, qualname string) string {
	return DeriveID(key, "symbol", repo, lang, qualname)
}
