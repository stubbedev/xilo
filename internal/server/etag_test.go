package server

import (
	"strings"
	"testing"
)

// The CDN-staleness regression: ETag must be derived from response content,
// so a re-push upsert (new NarHash, same store hash) and a key rotation (new
// signature in the narinfo body) both change it. A store-hash ETag cannot
// distinguish either — a proxy would pin stale bytes forever under
// Cache-Control: immutable.
func TestContentETag(t *testing.T) {
	narinfoA := contentETag("sha256:aaaa", "cache:KEY1")
	narinfoB := contentETag("sha256:bbbb", "cache:KEY1")   // re-push, new content
	narinfoRot := contentETag("sha256:aaaa", "cache:KEY2") // key rotation
	if narinfoA == narinfoB {
		t.Fatal("ETag identical across content change")
	}
	if narinfoA == narinfoRot {
		t.Fatal("ETag identical across key rotation")
	}
	if narinfoA == contentETag("sha256:aaaa", "cache:KEY1") {
		// determinism (same inputs → same tag) — this must NOT fail
	} else {
		t.Fatal("ETag not deterministic")
	}

	// NAR variant: keyed purely on the NAR hash, quoted, no sha256: prefix.
	nar := contentETag("sha256:cccc", "")
	if nar != `"cccc"` {
		t.Fatalf("nar etag = %s", nar)
	}
	for _, e := range []string{narinfoA, narinfoB, nar} {
		if !strings.HasPrefix(e, `"`) || !strings.HasSuffix(e, `"`) {
			t.Fatalf("unquoted etag %s", e)
		}
	}
}
