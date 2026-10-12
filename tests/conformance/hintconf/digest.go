package hintconf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	jcs "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// Canonical returns the RFC 8785 (JCS) form of v.
func Canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(b)
}

// Digest returns "sha256:" + hex(SHA-256(JCS(v))).
func Digest(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:]), nil
}

// SnapshotDigest computes the digest of one snapshot inside a repository_snapshot payload
// (hint-data §5.2): the snapshot record without its "digest" field, and the entities,
// relations and constraints whose snapshot_id is that snapshot, in payload order.
func SnapshotDigest(payload map[string]any, snapshotID string) (string, error) {
	var snap map[string]any
	for _, s := range list(payload["snapshots"]) {
		if m, ok := s.(map[string]any); ok && m["snapshot_id"] == snapshotID {
			snap = map[string]any{}
			for k, v := range m {
				if k != "digest" {
					snap[k] = v
				}
			}
		}
	}
	pick := func(key string) []any {
		out := []any{}
		for _, r := range list(payload[key]) {
			if m, ok := r.(map[string]any); ok && m["snapshot_id"] == snapshotID {
				out = append(out, m)
			}
		}
		return out
	}
	return Digest(map[string]any{
		"snapshot": snap, "entities": pick("entities"), "relations": pick("relations"),
		"constraints": pick("constraints"),
	})
}

func list(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
