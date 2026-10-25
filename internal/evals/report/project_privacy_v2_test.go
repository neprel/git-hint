package report

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Literal vectors were independently calculated with Python stdlib HMAC-SHA256:
// HMAC(key, b"hint:v0.1:eval-report:project-v2-" + role +
//
//	b"\x00" + part1 [+ b"\x00" + part2]).digest()[:16].hex().
//
// These are synthetic keys/identities, not source or producer authority.
var projectV2VectorKeys = [2]string{
	"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
	"a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf",
}
var projectV2VectorDigests = [14]string{
	"sha256:31356abdf94f1ad8f4c82e8cc20921a756a0cea6524237c42c31a1665644483c",
	"sha256:6ab5eac7eae79802721a9440e53e14d6296d9eeb97f241dc33b438e5fd5fb430",
	"sha256:a5b2e242b049f4df905ba7bc66dbb065acbb6928207ff514eb3c875f29427a0b",
	"sha256:212a1b24e3abcd0df1bacace9726f89b48aa61ab345004908cab8bbb1fce79f8",
	"sha256:ebf4d807b866d04bbaa6a5c2d7d841d22f342c4ece230523e5e8712cd1a408c7",
	"sha256:b9b7aa57414dbfa4f7be3fddde98983b3a2bd4096d99b8d427b9bbd8b5819af9",
	"sha256:6061f375d36977e343df530dca2ac86f073e0e7b7fb10fb830f2b2e54ddf7461",
	"sha256:f2fe8bfaee6ccaa33b5e3a8f10f13bf7b0d841ed56843ce1686b03152b15254f",
	"sha256:2d04290ca2653edfc55b19083c9292b12715d8e9041142b41654cbd24ea3265a",
	"sha256:700cb724d2116ba85ea40d9329ab29704ee3ef6437f096097912029f221df05b",
	"sha256:34c912a3ddfbb2963c4a752f68f58eeef377734b2e9c1e0071b3514014838704",
	"sha256:c74a78c72a0d472aa50f18a896061f03b2e0abd1e2affba5ec7abddc02828499",
	"sha256:36f765910894b777293062c607faa5153eea11f5ff0baba9a73ba3f200d641ca",
	"sha256:b86ed20809ab091a5775c34dd81f7c008552d4438f4fee1964aef436a39514d2",
}
var projectV2Expected = [2][14]string{
	{"338a6166b791dbe5405fda1b2d43bd2f", "35b49cb16ddf7af1516bb5b5d8984b9b", "94851d65e7b25fea5134bcc56b994459", "f6f01dbbc553e0609f2db1c33b969b5b", "2b50927f5495a27a6eca6c9ce1b9dec7", "553a61ee02c6f5cb81c59f1f53393dc8", "3ca34eba22716d0fbbd0cd3b50478851", "c68c5d59183cdb237738173cfe86cd41", "42d52a0e222ee303bc9b8e367f4a699c", "8cf291b9d3eab358c083d781b4a17df4", "8b7742b29fd7eb76d3f1b2931ed52625", "cd40eb34e6ae862a688927225e0a10c2", "2053cc5b35cc247a43a531b111d9e960", "e62f7c4e77be18a9a64c51ed79229519"},
	{"f844b53965a1b0f708b626483cca7f23", "e3448363e55184d333a021cc20754ca2", "0049b8d5879bce5ee7976c6321be8138", "d845e0106957e09ed395a1783383750a", "eb69671e3f465597a08d86ba46fe5b11", "9044eab1bfc34f4fb8337a51c3e8217f", "7bb8421daee61c1642e9ce336630b528", "756738b0de28dbc4a84cfcee01f5f5e0", "ec13183d22fa6064a8531b0a79466b95", "8bddb541133b6d65b4228154faa61dfc", "54df99c1a0bea1aa982c65d731ab2cde", "02cee6c8f05567a6365fd5fa915e9bd9", "141a0ef681c2fd57606a6d3821ef41f9", "71def26d202afbc07f4390db726bc87f"},
}

func projectV2Fixture() ProjectV2LocalPrivacyInput {
	d := projectV2VectorDigests
	return ProjectV2LocalPrivacyInput{Intent: ProjectV2IntentPrivacyInput{
		Definition: ProjectV2RecordVersion{"8e5a78987ede623a376ead3161986acd", d[0]},
		// Unit/operation use independent existing producer vectors, including the
		// existing single-part UnitRef+NUL+OperationID preparation recipe.
		ReferencePreparation: ProjectV2RecordVersion{"ba9a91ea4a640b79f01b57648e35528e.d7058a53df5a0e694866a19590372bf3.outcome", d[1]},
		// No genuine capture producer exists. These two lexical vectors represent
		// synthetic privacy inputs only, never accepted source records.
		CapturePolicy: ProjectV2RecordVersion{"4c4b87620f4053e91ce5a575cfaf5d3f", d[2]},
		CaptureSource: ProjectV2RecordVersion{"d303d4ed442c3221e7e83510ea3640e0", d[3]},
		Spec:          d[4], Inputs: d[5], ReferencePatchArchive: d[6], PreparedReferenceTAR: d[7], SelectedStartTree: d[8],
	}, Verification: &ProjectV2VerificationPrivacyInput{d[9], d[10], d[11], d[12], d[13]}}
}
func projectV2Key(t *testing.T, n int) []byte {
	t.Helper()
	b, e := hex.DecodeString(projectV2VectorKeys[n])
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func projectV2Flat(v ProjectV2PrivacyRefs) []string {
	i := v.Intent
	out := []string{i.DefinitionVersionRef, i.ReferencePreparationVersionRef, i.CapturePolicyVersionRef, i.CaptureSourceVersionRef, i.SpecRef, i.InputsRef, i.ReferenceInputRef, i.ReferenceArtifactRef, i.BaseHintTreeRef}
	if v.Verification != nil {
		p := v.Verification
		out = append(out, p.ArtifactRef, p.EvaluatorImageRef, p.CandidateOutputsRef, p.OracleOutputsRef, p.EvaluatorResultRef)
	}
	return out
}
func projectV2DigestFields(in *ProjectV2LocalPrivacyInput) []*string {
	i := &in.Intent
	v := []*string{&i.Definition.RecordDigest, &i.ReferencePreparation.RecordDigest, &i.CapturePolicy.RecordDigest, &i.CaptureSource.RecordDigest, &i.Spec, &i.Inputs, &i.ReferencePatchArchive, &i.PreparedReferenceTAR, &i.SelectedStartTree}
	if p := in.Verification; p != nil {
		v = append(v, &p.ResultArtifact, &p.EvaluatorImage, &p.CandidateOutputs, &p.OracleOutputs, &p.EvaluatorResult)
	}
	return v
}
func projectV2RecordFields(in *ProjectV2LocalPrivacyInput) []*string {
	i := &in.Intent
	return []*string{&i.Definition.RecordRef, &i.ReferencePreparation.RecordRef, &i.CapturePolicy.RecordRef, &i.CaptureSource.RecordRef}
}

func TestProjectV2PrivacyLiteralVectors(t *testing.T) {
	for n := 0; n < 2; n++ {
		key := projectV2Key(t, n)
		before := bytes.Clone(key)
		in := projectV2Fixture()
		got, e := RenderProjectV2Privacy(key, in)
		if e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(projectV2Flat(got), projectV2Expected[n][:]) {
			t.Fatal("independent literal HMAC vector mismatch", n, projectV2Flat(got))
		}
		if !bytes.Equal(key, before) {
			t.Fatal("renderer changed caller key")
		}
		for _, ref := range projectV2Flat(got) {
			if !projectV2LowerHex(ref, 32) {
				t.Fatal("non Ref32 output")
			}
		}
		in.Verification = nil
		intent, e := RenderProjectV2Privacy(key, in)
		if e != nil || intent.Verification != nil || !reflect.DeepEqual(projectV2Flat(intent), projectV2Expected[n][:9]) {
			t.Fatal("intent-only mismatch", intent, e)
		}
	}
	for i := 0; i < 14; i++ {
		if projectV2Expected[0][i] == projectV2Expected[1][i] {
			t.Fatal("key separation lost", i)
		}
	}
}

func TestProjectV2PrivacyFieldAndRoleSeparation(t *testing.T) {
	key := projectV2Key(t, 0)
	for index := 0; index < 14; index++ {
		in := projectV2Fixture()
		*projectV2DigestFields(&in)[index] = "sha256:" + strings.Repeat("f", 64)
		got, e := RenderProjectV2Privacy(key, in)
		if e != nil {
			t.Fatal(e)
		}
		for i, ref := range projectV2Flat(got) {
			if (ref != projectV2Expected[0][i]) != (i == index) {
				t.Fatal("digest changed wrong binding", index, i)
			}
		}
	}
	for index := 0; index < 4; index++ {
		in := projectV2Fixture()
		ref := strings.Repeat("1", 32)
		if index == 1 {
			ref = ref + "." + strings.Repeat("2", 32) + ".outcome"
		}
		*projectV2RecordFields(&in)[index] = ref
		got, e := RenderProjectV2Privacy(key, in)
		if e != nil {
			t.Fatal(e)
		}
		for i, ref := range projectV2Flat(got) {
			if (ref != projectV2Expected[0][i]) != (i == index) {
				t.Fatal("record ref changed wrong binding", index, i)
			}
		}
	}
	in := projectV2Fixture()
	// Independent SHA256 of literal canonical ["B1"]. As a private-role input,
	// even these coincidentally equal bytes MUST receive the role's keyed binding.
	same := "sha256:e6b0d5781db025e9f118f798551a863f03de0b19e24a7cec4522a7a4626297ff"
	for _, p := range projectV2DigestFields(&in) {
		*p = same
	}
	in.Intent.Definition.RecordRef = strings.Repeat("b", 32)
	in.Intent.CapturePolicy.RecordRef = in.Intent.Definition.RecordRef
	in.Intent.CaptureSource.RecordRef = in.Intent.Definition.RecordRef
	got, e := RenderProjectV2Privacy(key, in)
	if e != nil {
		t.Fatal(e)
	}
	if got.Intent.SpecRef != "9aad46ebe323e50f33d42f788e137efa" {
		t.Fatal("canonical-looking private input bypassed role HMAC")
	}
	seen := map[string]bool{}
	for _, ref := range projectV2Flat(got) {
		if seen[ref] {
			t.Fatal("different semantic roles collided")
		}
		seen[ref] = true
	}
	// A different well-formed preparation unit component changes its binding.
	// With no enclosing UnitRef input, this pure helper cannot certify unit equality.
	in = projectV2Fixture()
	in.Intent.ReferencePreparation.RecordRef = strings.Repeat("c", 32) + in.Intent.ReferencePreparation.RecordRef[32:]
	if _, e := RenderProjectV2Privacy(key, in); e != nil {
		t.Fatal("lexical mapping claimed enclosing-unit authority", e)
	}
}

func TestProjectV2PrivacySerializedRefsHideEveryPrivateInput(t *testing.T) {
	for _, full := range []bool{false, true} {
		in := projectV2Fixture()
		if !full {
			in.Verification = nil
		}
		out, e := RenderProjectV2Privacy(projectV2Key(t, 0), in)
		if e != nil {
			t.Fatal(e)
		}
		wire, e := json.Marshal(out)
		if e != nil {
			t.Fatal(e)
		}
		var sentinels []string
		for _, p := range projectV2DigestFields(&in) {
			sentinels = append(sentinels, *p, (*p)[7:], (*p)[7:39], (*p)[7:15])
		}
		for _, p := range projectV2RecordFields(&in) {
			sentinels = append(sentinels, *p, (*p)[:8])
		}
		sentinels = append(sentinels, strings.Split(in.Intent.ReferencePreparation.RecordRef, ".")[1], projectV2VectorKeys[0], "synthetic-unit-private-label", "synthetic-operation-private-label", "private-policy-label", "private-source-label", "/private/native/path", "model-secret", "config-secret")
		for _, value := range sentinels {
			if bytes.Contains(wire, []byte(value)) {
				t.Fatal("private sentinel or unkeyed alias leaked")
			}
		}
		if full && len(projectV2Flat(out)) != 14 || !full && len(projectV2Flat(out)) != 9 {
			t.Fatal("wrong output inventory")
		}
	}
}

func TestProjectV2PrivacyAllOrNothingAndStaticErrors(t *testing.T) {
	zero := ProjectV2PrivacyRefs{}
	check := func(key []byte, in ProjectV2LocalPrivacyInput) {
		t.Helper()
		got, e := RenderProjectV2Privacy(key, in)
		if e == nil || !reflect.DeepEqual(got, zero) {
			t.Fatal("malformed input returned partial refs", e)
		}
		if e != ErrProjectV2PrivacyKey && e != ErrProjectV2PrivacyRecord && e != ErrProjectV2PrivacyDigest {
			t.Fatal("nonstatic error")
		}
		if strings.Contains(e.Error(), "sha256:") || strings.Contains(e.Error(), "/") {
			t.Fatal("private data in error")
		}
	}
	for _, n := range []int{0, 1, 31, 33, 64} {
		check(bytes.Repeat([]byte{1}, n), projectV2Fixture())
	}
	check(make([]byte, 32), projectV2Fixture())
	for index := 0; index < 14; index++ {
		for _, bad := range []string{"", "sha256:", strings.ToUpper(projectV2VectorDigests[index]), " " + projectV2VectorDigests[index], projectV2VectorDigests[index] + "\n", "sha256:" + strings.Repeat("0", 63), "sha256:" + strings.Repeat("g", 64), "sha256:" + strings.Repeat("a", 63) + "\x00", strings.Repeat("x", 1<<20)} {
			in := projectV2Fixture()
			*projectV2DigestFields(&in)[index] = bad
			check(projectV2Key(t, 0), in)
		}
	}
	for index := 0; index < 4; index++ {
		for _, bad := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32), strings.Repeat("g", 32), "/private/native-record", strings.Repeat("a", 31) + "\x00", strings.Repeat("x", 1<<20)} {
			in := projectV2Fixture()
			*projectV2RecordFields(&in)[index] = bad
			check(projectV2Key(t, 0), in)
		}
	}
	for _, bad := range []string{strings.Repeat("a", 32) + ".outcome", strings.Repeat("a", 31) + "." + strings.Repeat("b", 32) + ".outcome", strings.Repeat("a", 32) + "." + strings.Repeat("b", 31) + ".outcome", strings.Repeat("a", 32) + "." + strings.Repeat("b", 32) + ".request", strings.Repeat("a", 32) + "." + strings.Repeat("b", 32) + ".reservation", strings.Repeat("a", 32) + "." + strings.Repeat("b", 32) + ".Outcome"} {
		in := projectV2Fixture()
		in.Intent.ReferencePreparation.RecordRef = bad
		check(projectV2Key(t, 0), in)
	}
	in := projectV2Fixture()
	in.Verification = &ProjectV2VerificationPrivacyInput{}
	check(projectV2Key(t, 0), in)
}

func TestProjectV2PrivacyCopiesHaveNoInputOrKeyAlias(t *testing.T) {
	key := projectV2Key(t, 0)
	in := projectV2Fixture()
	one, e := RenderProjectV2Privacy(key, in)
	if e != nil {
		t.Fatal(e)
	}
	two, e := RenderProjectV2Privacy(key, in)
	if e != nil {
		t.Fatal(e)
	}
	if one.Verification == two.Verification {
		t.Fatal("optional output pointer reused")
	}
	for i := range key {
		key[i] ^= 255
	}
	in.Verification.ResultArtifact = "sha256:" + strings.Repeat("e", 64)
	in.Intent.Spec = "sha256:" + strings.Repeat("f", 64)
	if !reflect.DeepEqual(projectV2Flat(one), projectV2Expected[0][:]) {
		t.Fatal("returned refs retained input or key")
	}
	one.Verification.ArtifactRef = "changed"
	if !reflect.DeepEqual(projectV2Flat(two), projectV2Expected[0][:]) {
		t.Fatal("optional result pointer shared")
	}
	again, e := RenderProjectV2Privacy(projectV2Key(t, 0), projectV2Fixture())
	if e != nil || !reflect.DeepEqual(projectV2Flat(again), projectV2Expected[0][:]) {
		t.Fatal("renderer retained mutable state", e)
	}
}
