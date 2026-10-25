package project

import "testing"

// Advertised versions must not promote ordinary or earlier-draft packages.
func TestWriterMetadataPreservesLegacyPackageSelection(t *testing.T) {
	s, b := batchFixture(t, "metadata-legacy-selection")
	if got, err := packageVersion(s, ExportOptions{All: true}); err != nil || got != "0.1.0" {
		t.Fatalf("ordinary: %q %v", got, err)
	}
	if err := WriteActivityBatch(s, b.Source, b.Observations); err != nil {
		t.Fatal(err)
	}
	if got, err := packageVersion(s, ExportOptions{All: true}); err != nil || got != "0.2.0" {
		t.Fatalf("CLI-root draft: %q %v", got, err)
	}
}
