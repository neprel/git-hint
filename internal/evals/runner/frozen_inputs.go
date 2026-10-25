package runner

// These are LOCAL content handles. They must not be projected directly to a
// report: normal wire privacy needs a separately reviewed keyed projection.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

var ErrFrozenInputs = errors.New("frozen verification inputs refused")

type FrozenIOInputs struct {
	task                *ioTask
	limits              taskLimits
	image, configDigest string
	sources             map[string]string
}
type FrozenIOInputsIdentity struct {
	Cases                                                     []string
	CaseSeconds                                               int
	SpecDigest, InputsDigest, DeclaredImage, TaskConfigDigest string
	VerifierLimit                                             time.Duration
	CPUs                                                      float64
	MemoryMB                                                  int64
	Network                                                   string
	SourceInputs                                              map[string]string
}

func cloneStringMap(m map[string]string) map[string]string {
	v := map[string]string{}
	for k, s := range m {
		v[k] = s
	}
	return v
}
func cloneIOTask(t *ioTask) *ioTask {
	if t == nil {
		return nil
	}
	v := *t
	v.spec.Entry = append([]string(nil), t.spec.Entry...)
	v.spec.Cases = append([]string(nil), t.spec.Cases...)
	v.inputs = map[string][]byte{}
	for k, b := range t.inputs {
		v.inputs[k] = bytes.Clone(b)
	}
	return &v
}
func (h *FrozenIOInputs) valid() bool {
	return h != nil && h.task != nil && len(h.image) > 0 && len(h.image) <= 4096 && utf8.ValidString(h.image) && referencePolicySHA.MatchString(h.configDigest) && len(h.sources) > 0
}
func (t *Task) FrozenIOInputs() (*FrozenIOInputs, error) {
	if t == nil || !t.frozenPublished || !t.frozen.valid() {
		return nil, ErrFrozenInputs
	}
	v := *t.frozen
	v.task = cloneIOTask(v.task)
	v.sources = cloneStringMap(v.sources)
	return &v, nil
}
func (h *FrozenIOInputs) Identity() FrozenIOInputsIdentity {
	if !h.valid() {
		return FrozenIOInputsIdentity{}
	}
	net := "none"
	if h.limits.lan {
		net = "allowlist"
	}
	return FrozenIOInputsIdentity{SpecDigest: h.task.specDigest, InputsDigest: h.task.inputsDigest, DeclaredImage: h.image, TaskConfigDigest: h.configDigest, VerifierLimit: h.limits.verifier, CPUs: h.limits.cpus, MemoryMB: h.limits.memoryMB, Network: net, SourceInputs: cloneStringMap(h.sources), Cases: append([]string(nil), h.task.spec.Cases...), CaseSeconds: h.task.spec.CaseSeconds}
}
func (h *FrozenIOInputs) stamp() string { return referenceJSON(h.Identity()) }
func (h *FrozenIOInputs) byteCount() int64 {
	metadata, _ := json.Marshal(h.Identity())
	n := int64(len(metadata))
	for _, arg := range h.task.spec.Entry {
		n += int64(len(arg))
	}
	for _, b := range h.task.inputs {
		n += int64(len(b))
	}
	return n
}

type FrozenIOArtifactIdentity struct {
	Domain, Digest string
	Bytes          int64
}
type FrozenIOArtifact struct {
	identity FrozenIOArtifactIdentity
	data     []byte
}

func (h *FrozenIOArtifact) Identity() FrozenIOArtifactIdentity {
	if h == nil {
		return FrozenIOArtifactIdentity{}
	}
	return h.identity
}
func (h *FrozenIOArtifact) valid() bool {
	return h != nil && h.identity.Domain == referenceTARDomain && h.identity.Bytes > 0 && h.identity.Bytes <= 96<<20 && int64(len(h.data)) == h.identity.Bytes && referenceDigest(h.data) == h.identity.Digest
}

// ValidateFrozenIOArtifact validates bytes through the EXISTING tree validator.
// It grants no source, qualification, start or runtime authority.
func ValidateFrozenIOArtifact(parent context.Context, expected FrozenIOArtifactIdentity, data []byte) (handle *FrozenIOArtifact, err error) {
	if parent == nil || parent.Err() != nil || expected.Domain != referenceTARDomain || expected.Bytes <= 0 || expected.Bytes > 96<<20 || int64(len(data)) != expected.Bytes || !referencePolicySHA.MatchString(expected.Digest) {
		return nil, ErrFrozenInputs
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	owned := bytes.Clone(data)
	if referenceDigest(owned) != expected.Digest {
		return nil, ErrFrozenInputs
	}
	if ctx.Err() != nil {
		return nil, ErrFrozenInputs
	}
	dir, e := os.MkdirTemp("", "hint-frozen-input-")
	if e != nil {
		return nil, ErrFrozenInputs
	}
	defer func() {
		if e := os.RemoveAll(dir); e != nil || ctx.Err() != nil {
			handle = nil
			err = ErrFrozenInputs
		}
	}()
	digest, e := freezeTreeWithLimits(&referenceReader{ctx: ctx, r: bytes.NewReader(owned)}, filepath.Join(dir, "tree.tar"), 96<<20, 96<<20)
	if e != nil || digest != expected.Digest || ctx.Err() != nil {
		return nil, ErrFrozenInputs
	}
	return &FrozenIOArtifact{expected, owned}, nil
}

type FrozenIOReferenceTranscript struct {
	inputs, digest string
	cases          []IOTranscriptCase
}
type FrozenIOReferenceTranscriptIdentity struct {
	DeclarationBinding, Digest string
	Cases                      int
}

func (h *FrozenIOReferenceTranscript) Identity() FrozenIOReferenceTranscriptIdentity {
	if h == nil {
		return FrozenIOReferenceTranscriptIdentity{}
	}
	return FrozenIOReferenceTranscriptIdentity{h.inputs, h.digest, len(h.cases)}
}

func ValidateFrozenIOReferenceTranscript(parent context.Context, inputs *FrozenIOInputs, cases []IOTranscriptCase, expectedDigest string) (*FrozenIOReferenceTranscript, error) {
	if parent == nil || parent.Err() != nil || !inputs.valid() || len(cases) != len(inputs.task.spec.Cases) || !referencePolicySHA.MatchString(expectedDigest) {
		return nil, ErrFrozenInputs
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	v := make([]IOTranscriptCase, len(cases))
	for i, c := range cases {
		if ctx.Err() != nil || c.ID != inputs.task.spec.Cases[i] || c.Status != 0 || len(c.Output) > maxIOBytes {
			return nil, ErrFrozenInputs
		}
		v[i] = IOTranscriptCase{c.ID, c.Status, bytes.Clone(c.Output)}
	}
	digest, e := IOTranscriptDigest(v)
	if e != nil || digest != expectedDigest || ctx.Err() != nil {
		return nil, ErrFrozenInputs
	}
	return &FrozenIOReferenceTranscript{inputs.stamp(), digest, v}, nil
}
