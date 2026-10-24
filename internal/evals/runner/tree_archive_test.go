package runner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completeTestTree(t *testing.T, format tar.Format, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range []*tar.Header{
		{Name: "app/", Typeflag: tar.TypeDir, Mode: 0755, Format: format},
		{Name: "app/" + name, Typeflag: tar.TypeReg, Mode: 0644, Size: 3, Uid: 11, Gid: 12, ModTime: time.Unix(17, 0), Format: format},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFreezeTreeCompleteLegacyFormatsPreserveBytesAndDigest(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatUSTAR, tar.FormatPAX, tar.FormatGNU} {
		t.Run(format.String(), func(t *testing.T) {
			name := "file.go"
			if format != tar.FormatUSTAR {
				name = strings.Repeat("long-name-", 30) + ".go"
			}
			canonical := completeTestTree(t, format, name)
			hash := sha256.Sum256(canonical)
			for _, extra := range []int{0, 512, 20 * 512} {
				raw := append(bytes.Clone(canonical), make([]byte, extra)...)
				dest := filepath.Join(t.TempDir(), "tree.tar")
				digest, err := freezeTree(bytes.NewReader(raw), dest)
				got, readErr := os.ReadFile(dest)
				if err != nil || readErr != nil || !bytes.Equal(got, canonical) || digest != "sha256:"+hex.EncodeToString(hash[:]) {
					t.Fatalf("format %s padding %d digest %s error %v %v", format, extra, digest, err, readErr)
				}
			}
		})
	}
}

func TestFreezeTreeRejectsEveryTruncationAndNonzeroTrailer(t *testing.T) {
	good := completeTestTree(t, tar.FormatUSTAR, "file.go")
	cases := map[string][]byte{}
	// Every shortened byte prefix includes payload, padding, entry boundaries and both
	// EOS blocks. None is a complete archive, even when archive/tar returns plain EOF.
	path := filepath.Join(t.TempDir(), "cut.tar")
	for cut := 0; cut < len(good); cut++ {
		if _, err := freezeTree(bytes.NewReader(good[:cut]), path); err == nil {
			t.Fatalf("accepted prefix %d of %d", cut, len(good))
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("prefix %d left artifact", cut)
		}
	}
	cases["nonzero payload padding"] = bytes.Clone(good)
	cases["nonzero payload padding"][1027] = 1
	cases["nonzero EOS"] = bytes.Clone(good)
	cases["nonzero EOS"][len(good)-512] = 1
	cases["trailing data"] = append(bytes.Clone(good), 1)
	cases["partial zero record padding"] = append(bytes.Clone(good), make([]byte, 511)...)
	cases["nonzero record padding"] = append(bytes.Clone(good), append(make([]byte, 511), 1)...)
	cases["concatenated archive"] = append(bytes.Clone(good), good...)
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.tar")
			if _, err := freezeTree(bytes.NewReader(raw), path); err == nil {
				t.Fatal("accepted malformed archive")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("failed archive left artifact")
			}
		})
	}
}

type countedTreeInput struct {
	reader io.Reader
	read   int
}

func (r *countedTreeInput) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestFreezeTreeRawBoundIncludesMetadataAndZeroTrailer(t *testing.T) {
	long := completeTestTree(t, tar.FormatPAX, strings.Repeat("metadata-", 100)+".go")
	good := completeTestTree(t, tar.FormatUSTAR, "file.go")
	for name, raw := range map[string][]byte{"PAX metadata": long, "zero trailer": append(bytes.Clone(good), make([]byte, 4096)...)} {
		t.Run(name, func(t *testing.T) {
			limit := int64(len(good))
			if name == "PAX metadata" {
				limit = 1024
			}
			counted := &countedTreeInput{reader: bytes.NewReader(raw)}
			path := filepath.Join(t.TempDir(), "bounded.tar")
			if _, err := freezeTreeBounded(counted, path, limit); err == nil {
				t.Fatal("raw limit ignored")
			}
			if int64(counted.read) > limit+1 {
				t.Fatalf("unbounded read %d limit %d", counted.read, limit)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("raw limit left artifact")
			}
		})
	}
	// Exact raw limit remains valid; the one-byte overflow probe does not reject EOF.
	if _, err := freezeTreeBounded(bytes.NewReader(good), filepath.Join(t.TempDir(), "exact.tar"), int64(len(good))); err != nil {
		t.Fatal(err)
	}
}
