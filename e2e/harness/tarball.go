package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

// BuildFakeReleaseTarball creates an in-memory .tar.gz with a single file
// named "che" whose contents are the given bytes. Used by the direct-install
// upgrade test to validate that che downloaded, extracted, and installed the
// right payload.
func BuildFakeReleaseTarball(t *testing.T, content []byte) []byte {
	t.Helper()
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	tw := tar.NewWriter(gz)

	hdr := &tar.Header{
		Name: "che",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tarball: write header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tarball: write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tarball: close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("tarball: close gzip: %v", err)
	}
	return gzBuf.Bytes()
}
