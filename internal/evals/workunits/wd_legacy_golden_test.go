package workunits

// Frozen from the original producer before the neutral DTO/helper factoring.
// These synthetic bytes test serialization and normalization, not runtime authority.
import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

func TestWorkDefinitionLegacyProducerBytesUnchanged(t *testing.T) {
	for _, harness := range []string{"", "hint-docker"} {
		m := Manifest{RunID: "jb-0000000000000042", Suite: "su-0000000000000042", Agent: "opencode", Arm: "baseline", Attempts: 2, RunnerVersion: "0.1.0", ModelLabel: "synthetic", LaunchDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Harness: harness, CreatedAt: "2026-10-25T00:00:00Z", Members: []Member{}}
		cfg := ScopeConfiguration([]byte("synthetic-key"), m)
		u := Unit{Eval: "ev-0000000000000042@1", UnitRef: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scope: map[string]any{"configuration": cfg, "declared_at": "2026-10-25T00:00:00Z"}, State: "open", Attempts: []map[string]any{}, Trials: []string{}}
		if e := u.Revise(); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(struct {
			Manifest Manifest `json:"manifest"`
			Units    []*Unit  `json:"units"`
		}{m, []*Unit{&u}})
		want := map[string]string{"": "8eceeefe7370e7caee84e1f32b772c57a23fc23f131234ce07933e81890e9255", "hint-docker": "8073026c8d9b429c280cae01e7e68f13027fb4f9e5c11ad0b916b7f9c09d6d5b"}[harness]
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
			t.Fatalf("legacy producer bytes changed for harness %q: %s", harness, got)
		}
	}
}
