// Package bundle moves eval versions between the user's own machines: a sensitive local
// file with the eval records and every object they reference. A bundle is never sent to a
// HINT server and is separate from the canonical export.
package bundle

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neprel/git-hint/internal/evals"
	"github.com/neprel/git-hint/internal/evals/library"
)

// Version of the bundle format.
const Version = 1

// Limits of one bundle.
const (
	MaxEntries    = 200000
	MaxEntryBytes = 512 << 20
)

// Index is bundle.json.
type Index struct {
	BundleVersion int      `json:"bundle_version"`
	Evals         []string `json:"evals"`
	Objects       []string `json:"objects"`
}

var digestRe = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
var refRe = regexp.MustCompile(`^ev-[0-9a-f]{16}@[1-9][0-9]*$`)

// Pack writes the given eval versions (ID@version) with their objects. Objects that a tree
// manifest references are included too.
func Pack(l *library.Library, refs []string, w io.Writer) (Index, error) {
	idx := Index{BundleVersion: Version}
	objs := map[string]bool{}
	var manifests [][2]string
	sort.Strings(refs)
	for _, ref := range refs {
		if !refRe.MatchString(ref) {
			return idx, fmt.Errorf("invalid eval version %q (use ID@version)", ref)
		}
		var rec map[string]any
		if err := l.Manifest("eval", ref, &rec); err != nil {
			return idx, fmt.Errorf("no eval %s", ref)
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		manifests = append(manifests, [2]string{ref, string(append(b, '\n'))})
		idx.Evals = append(idx.Evals, ref)
		walk(rec, "", objs)
	}
	// follow tree manifests to their file objects
	for d := range objs {
		b, err := l.GetObject(d)
		if err != nil {
			continue // missing objects are reported below
		}
		if strings.HasPrefix(string(b), `{"tree_version":1`) {
			for _, x := range digestRe.FindAllString(string(b), -1) {
				objs[x] = true
			}
		}
	}
	for d := range objs {
		idx.Objects = append(idx.Objects, d)
	}
	sort.Strings(idx.Objects)
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: epoch, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	ib, _ := json.MarshalIndent(idx, "", "  ")
	if err := put("bundle.json", ib); err != nil {
		return idx, err
	}
	for _, m := range manifests {
		if err := put("manifests/eval/"+m[0]+".json", []byte(m[1])); err != nil {
			return idx, err
		}
	}
	missing := 0
	for _, d := range idx.Objects {
		b, err := l.GetObject(d)
		if err != nil {
			missing++
			continue
		}
		if err := put("objects/"+strings.TrimPrefix(d, "sha256:"), b); err != nil {
			return idx, err
		}
	}
	if err := tw.Close(); err != nil {
		return idx, err
	}
	if missing > 0 {
		return idx, fmt.Errorf("%d referenced objects are unavailable; the bundle is incomplete", missing)
	}
	return idx, nil
}

// walk collects the object digests that an eval needs to run. Session sources named in
// provenance locations ("source") are not shipped: they are whole sessions and not needed.
func walk(v any, key string, out map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			walk(y, k, out)
		}
	case []any:
		for _, y := range x {
			walk(y, key, out)
		}
	case string:
		if key != "source" && digestRe.MatchString(x) && len(x) == 71 {
			out[x] = true
		}
	}
}

// Result of an unpack.
type Result struct {
	Evals   []string `json:"evals"`
	Objects int      `json:"objects"`
	Same    int      `json:"already_present"`
}

// ErrBundle is returned for a bundle that fails a check; nothing of it is kept.
var ErrBundle = errors.New("the bundle is not valid")

var objName = regexp.MustCompile(`^objects/[0-9a-f]{64}$`)
var manName = regexp.MustCompile(`^manifests/eval/(ev-[0-9a-f]{16}@[1-9][0-9]*)\.json$`)

// Unpack reads a bundle, checks every object digest and every record, and stores them.
// Records are immutable: the same version with other content is refused.
func Unpack(l *library.Library, r io.Reader) (Result, error) {
	var res Result
	tr := tar.NewReader(r)
	var idx *Index
	objects := map[string][]byte{}
	records := map[string][]byte{}
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("%w: unreadable archive", ErrBundle)
		}
		if n++; n > MaxEntries || h.Size > MaxEntryBytes || h.Typeflag != tar.TypeReg {
			return res, fmt.Errorf("%w: unexpected entry", ErrBundle)
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxEntryBytes))
		if err != nil {
			return res, fmt.Errorf("%w: unreadable entry", ErrBundle)
		}
		switch {
		case h.Name == "bundle.json":
			idx = &Index{}
			if json.Unmarshal(data, idx) != nil || idx.BundleVersion != Version {
				return res, fmt.Errorf("%w: unsupported bundle version", ErrBundle)
			}
		case objName.MatchString(h.Name):
			d := "sha256:" + strings.TrimPrefix(h.Name, "objects/")
			if library.Digest(data) != d {
				return res, fmt.Errorf("%w: an object does not match its digest", ErrBundle)
			}
			objects[d] = data
		case manName.MatchString(h.Name):
			ref := manName.FindStringSubmatch(h.Name)[1]
			if err := evals.Validate("eval", data); err != nil {
				return res, fmt.Errorf("%w: record %s is not valid", ErrBundle, ref)
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			if fmt.Sprintf("%s@%d", m["eval_id"], int(m["version"].(float64))) != ref {
				return res, fmt.Errorf("%w: record name and content differ", ErrBundle)
			}
			records[ref] = data
		default:
			return res, fmt.Errorf("%w: unexpected entry name", ErrBundle)
		}
	}
	if idx == nil || len(idx.Evals) != len(records) {
		return res, fmt.Errorf("%w: index and records differ", ErrBundle)
	}
	for _, d := range idx.Objects {
		if _, ok := objects[d]; !ok {
			return res, fmt.Errorf("%w: a referenced object is missing", ErrBundle)
		}
	}
	for ref, data := range records {
		var m map[string]any
		json.Unmarshal(data, &m)
		need := map[string]bool{}
		walk(m, "", need)
		for d := range need {
			if _, ok := objects[d]; !ok {
				return res, fmt.Errorf("%w: record %s references a missing object", ErrBundle, ref)
			}
		}
		var old map[string]any
		if l.Manifest("eval", ref, &old) == nil {
			b, _ := json.MarshalIndent(old, "", "  ")
			if string(append(b, '\n')) != string(data) {
				return res, fmt.Errorf("%w: %s exists here with other content", library.ErrImmutable, ref)
			}
		}
	}
	// all checks passed: store objects first, records last
	for _, data := range objects {
		if _, err := l.PutObject(data); err != nil {
			return res, err
		}
		res.Objects++
	}
	refs := make([]string, 0, len(records))
	for ref := range records {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		var m map[string]any
		json.Unmarshal(records[ref], &m)
		if _, err := l.PutManifest("eval", m); err != nil {
			return res, err
		}
		res.Evals = append(res.Evals, ref)
	}
	return res, nil
}
