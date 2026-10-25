package hint

import (
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotPayloadPinnedDispatchAndNormalParity(t *testing.T) {
	schemas, e := LoadSchemas()
	if e != nil {
		t.Fatal(e)
	}
	_, doc := load(t, "../../examples/v0.1/single-repo.json")
	payload := doc["payload"]
	for _, v := range []string{"0.1.0", "0.1.7", "0.2.1", "0.3.0", "0.3.9"} {
		if e := schemas.ValidateSnapshotPayload(v, payload); e != nil {
			t.Fatal(v, e)
		}
	}
	for _, v := range []string{"0.4.0", "0.3", "0.3.evil", "0.3.0.extra", "0.3.-1"} {
		if e := schemas.ValidateSnapshotPayload(v, payload); e == nil {
			t.Fatal("unsupported version accepted", v)
		}
	}
	if e := schemas.ValidateSnapshotPayload("0.3.0", map[string]any{"extra": true}); e == nil {
		t.Fatal("non-payload accepted")
	}
	repos := []string{}
	for _, r := range list(doc["repositories"]) {
		repos = append(repos, r.(string))
	}
	p := payload.(map[string]any)
	compare := func() {
		t.Helper()
		want := []Finding{}
		for _, f := range Check(doc, nil) {
			if strings.HasPrefix(f.Path, "payload.snapshots") || strings.HasPrefix(f.Path, "payload.entities") || strings.HasPrefix(f.Path, "payload.relations") || strings.HasPrefix(f.Path, "payload.constraints") {
				want = append(want, f)
			}
		}
		got := CheckSnapshotPayload(doc["hint_version"].(string), p, repos)
		if len(got) != len(want) || len(got) > 0 && !reflect.DeepEqual(got, want) {
			t.Fatal("semantic wrapper diverges", got, want)
		}
	}
	compare()
	list(p["entities"])[0].(map[string]any)["repo_id"] = strings.Repeat("f", 32)
	compare()
	if len(CheckSnapshotPayload(doc["hint_version"].(string), p, repos)) == 0 {
		t.Fatal("invalid native graph accepted")
	}
}

func TestSnapshotPayloadGenericPrivacyAndQuantityParity(t *testing.T) {
	for _, mode := range []string{"privacy", "quantity"} {
		t.Run(mode, func(t *testing.T) {
			_, doc := load(t, "../../examples/v0.1/single-repo.json")
			p := doc["payload"].(map[string]any)
			entity := list(p["entities"])[0].(map[string]any)
			if mode == "privacy" {
				entity["name"] = "HINTCANARY synthetic private text"
			} else {
				entity["features"] = map[string]any{"size_bytes": map[string]any{"unit": "bytes", "state": "known", "value": float64(3), "bounds": map[string]any{"lower": float64(5), "upper": float64(10)}}}
			}
			repos := []string{}
			for _, r := range list(doc["repositories"]) {
				repos = append(repos, r.(string))
			}
			want := []Finding{}
			for _, f := range Check(doc, nil) {
				if strings.HasPrefix(f.Path, "payload.") {
					want = append(want, f)
				}
			}
			got := CheckSnapshotPayload(doc["hint_version"].(string), p, repos)
			if len(got) == 0 || !reflect.DeepEqual(got, want) {
				t.Fatal("normal payload checks diverge", got, want)
			}
		})
	}
}
