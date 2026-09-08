package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/testenv"
)

// TestS3RoundTrip drives a fakeS3 written to this package's own idea of the
// protocol, which cannot disagree with the client the way a real server does:
// SigV4 signing over a payload it actually verifies, real error codes and
// bodies, a real multi-object delete, streaming a body large enough to leave
// one buffer. Chunk bytes never touch the DB, so the blob backend is the only
// thing standing between a cache and its content, and "it works against our
// fake" is a weaker claim than the config file implies.
//
// Gated on XILO_S3_TEST_ENDPOINT (CI provides a MinIO service container).
// Locally:
//
//	docker run --rm -p 9000:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	XILO_S3_TEST_ENDPOINT=localhost:9000 go test ./internal/storage/ -run RealS3
func realS3(t *testing.T) *S3 {
	t.Helper()
	endpoint := os.Getenv("XILO_S3_TEST_ENDPOINT")
	if endpoint == "" {
		testenv.Service(t, "XILO_REQUIRE_S3", "a MinIO endpoint (XILO_S3_TEST_ENDPOINT)")
	}
	bucket := os.Getenv("XILO_S3_TEST_BUCKET")
	if bucket == "" {
		bucket = "xilo-test"
	}
	access := os.Getenv("XILO_S3_TEST_ACCESS_KEY")
	if access == "" {
		access = "minioadmin"
	}
	secret := os.Getenv("XILO_S3_TEST_SECRET_KEY")
	if secret == "" {
		secret = "minioadmin"
	}
	s, err := NewS3(config.S3{
		Endpoint:  endpoint,
		Bucket:    bucket,
		Region:    os.Getenv("XILO_S3_TEST_REGION"),
		AccessKey: access,
		SecretKey: secret,
		Insecure:  true,
	})
	if err != nil {
		t.Fatalf("NewS3 against %s: %v", endpoint, err)
	}
	return s
}

// uniqueKey keeps parallel runs and reruns off each other's objects: the
// bucket outlives any one test.
func uniqueKey(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return ChunkKey(hex.EncodeToString(b[:]))
}

func TestRealS3RoundTrip(t *testing.T) {
	s := realS3(t)
	ctx := context.Background()
	key := uniqueKey(t)
	t.Cleanup(func() { _ = s.Delete(ctx, key) })

	// Larger than one read buffer, so the body is genuinely streamed and the
	// signature covers a payload the server recomputes.
	data := make([]byte, 5<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put 5MiB: %v", err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("round trip changed bytes: put %d, got %d", len(data), len(got))
	}

	if ok, err := s.Has(ctx, key); err != nil || !ok {
		t.Fatalf("Has(existing) = %v, %v", ok, err)
	}
	if ok, err := s.Has(ctx, uniqueKey(t)); err != nil || ok {
		t.Fatalf("Has(missing) = %v, %v; a real 404 must map to absent, not an error", ok, err)
	}
	if _, err := s.Get(ctx, uniqueKey(t)); err == nil {
		t.Fatal("Get(missing) should error")
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Has(ctx, key); ok {
		t.Fatal("key still present after Delete")
	}
	// The GC sweeper deletes blobs that may already be gone, so a delete of a
	// missing key has to stay a success against a real server too.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
}

// DeleteMany is what the GC sweep uses, and its wire form is a signed POST
// with an XML body: the one operation whose request the fake does not really
// reproduce. A partial failure must also be reported rather than swallowed,
// or the sweep will think it reclaimed space it did not.
func TestRealS3DeleteMany(t *testing.T) {
	s := realS3(t)
	ctx := context.Background()

	keys := make([]string, 0, 5)
	for range 5 {
		k := uniqueKey(t)
		if err := s.Put(ctx, k, bytes.NewReader([]byte("sweep me"))); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
		keys = append(keys, k)
	}
	// A key that was never there: the sweep sees these constantly, since two
	// passes can both decide the same orphan is unreferenced.
	keys = append(keys, uniqueKey(t))

	if err := s.DeleteMany(ctx, keys); err != nil {
		t.Fatalf("DeleteMany: %v", err)
	}
	for _, k := range keys {
		if ok, err := s.Has(ctx, k); err != nil {
			t.Fatalf("Has(%s) after DeleteMany: %v", k, err)
		} else if ok {
			t.Fatalf("%s survived DeleteMany", k)
		}
	}

	// Empty input is a no-op, not a malformed request the server rejects.
	if err := s.DeleteMany(ctx, nil); err != nil {
		t.Fatalf("DeleteMany(nil): %v", err)
	}
}

// New() dispatches on the config, and an s3 backend that cannot reach its
// bucket should say so at construction rather than on the first push.
func TestRealS3ThroughNew(t *testing.T) {
	s := realS3(t)
	ctx := context.Background()

	st, err := New(config.Storage{Backend: "s3", S3: config.S3{
		Endpoint: os.Getenv("XILO_S3_TEST_ENDPOINT"),
		Bucket:   cmpOr(os.Getenv("XILO_S3_TEST_BUCKET"), "xilo-test"),
		// Not decoration: NewS3 falls back to us-east-1 for an empty region
		// because the signer needs one, and a server that checks the SigV4
		// scope (Garage does, MinIO does not) answers 400
		// AuthorizationHeaderMalformed to every request signed for the wrong
		// one. Dropping it here made this the only construction in the file
		// signing for a region the server never agreed to.
		Region:    os.Getenv("XILO_S3_TEST_REGION"),
		AccessKey: cmpOr(os.Getenv("XILO_S3_TEST_ACCESS_KEY"), "minioadmin"),
		SecretKey: cmpOr(os.Getenv("XILO_S3_TEST_SECRET_KEY"), "minioadmin"),
		Insecure:  true,
	}})
	if err != nil {
		t.Fatalf("New(s3): %v", err)
	}
	key := uniqueKey(t)
	t.Cleanup(func() { _ = s.Delete(ctx, key) })
	if err := st.Put(ctx, key, bytes.NewReader([]byte("via New"))); err != nil {
		t.Fatalf("Put through New: %v", err)
	}
	if ok, err := st.Has(ctx, key); err != nil || !ok {
		t.Fatalf("Has through New = %v, %v", ok, err)
	}
}

func cmpOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
