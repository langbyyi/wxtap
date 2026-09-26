package traffic

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompressSkipsSmallBodies(t *testing.T) {
	small := []byte(`{"ok":1}`)
	blob, err := compress(small)
	if err != nil {
		t.Fatalf("compress small: %v", err)
	}
	if !bytes.Equal(blob, small) {
		t.Fatalf("small body should be stored raw, got %d bytes", len(blob))
	}
}

func TestCompressRoundTripsLargeBodies(t *testing.T) {
	large := bytes.Repeat([]byte("payload-"), 512) // 4KiB, gzip-worthy
	blob, err := compress(large)
	if err != nil {
		t.Fatalf("compress large: %v", err)
	}
	if len(blob) >= len(large) {
		t.Fatalf("large body should be compressed: %d -> %d", len(large), len(blob))
	}
	got, err := decompressLimited(blob, MaxBodyBytes)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, large) {
		t.Fatal("round trip mismatch")
	}
}

func TestDecompressLimitedReturnsRawSmallBodiesUntouched(t *testing.T) {
	small := []byte(`{"ok":1}`)
	got, err := decompressLimited(small, MaxBodyBytes)
	if err != nil {
		t.Fatalf("decompress raw: %v", err)
	}
	if !bytes.Equal(got, small) {
		t.Fatalf("raw blob mutated: %q", got)
	}
}

func TestCompressNilStaysNil(t *testing.T) {
	blob, err := compress(nil)
	if err != nil || blob != nil {
		t.Fatalf("empty body: %v %v", blob, err)
	}
	got, err := decompressLimited(nil, MaxBodyBytes)
	if err != nil || got != nil {
		t.Fatalf("decompress nil: %v %v", got, err)
	}
}

// decompressLimited is the only decompression path left, so it must cap a
// hostile blob silently (an oversized payload is truncated, not an error) but
// still surface a truncated gzip stream instead of serving partial bytes as
// success — a cut-off archive means the stored body is damaged.
func TestDecompressLimitedBoundsAndRejectsTruncatedArchives(t *testing.T) {
	large := bytes.Repeat([]byte("payload-"), 512) // 4KiB, gzip-worthy
	blob, err := compress(large)
	if err != nil {
		t.Fatalf("compress large: %v", err)
	}

	got, err := decompressLimited(blob, 1024)
	if err != nil {
		t.Fatalf("over-limit decompress: %v", err)
	}
	if len(got) != 1024 || !bytes.Equal(got, large[:1024]) {
		t.Fatalf("over-limit decompress returned %d bytes, want the 1024-byte cap", len(got))
	}

	// A gzip stream with a cut-off trailer must error, matching the
	// "read gzip: ..." wording decompressLimited wraps read failures in.
	if _, err := decompressLimited(blob[:len(blob)-4], MaxBodyBytes); err == nil || !strings.Contains(err.Error(), "read gzip") {
		t.Fatalf("expected a gzip read error for a truncated archive, got %v", err)
	}
}
