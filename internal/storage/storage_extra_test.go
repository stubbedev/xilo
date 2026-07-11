package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/aws/smithy-go"

	"github.com/stubbedev/xilo/internal/config"
)

func TestNewDispatch(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		cfg     config.Storage
		wantErr bool
		local   bool
	}{
		{"default-local", config.Storage{Local: config.Local{Root: root}}, false, true},
		{"explicit-local", config.Storage{Backend: "local", Local: config.Local{Root: root}}, false, true},
		{"local-missing-root", config.Storage{Backend: "local"}, true, false},
		{"s3", config.Storage{Backend: "s3", S3: config.S3{Endpoint: "e", Bucket: "b"}}, false, false},
		{"s3-missing", config.Storage{Backend: "s3"}, true, false},
		{"invalid", config.Storage{Backend: "gcs"}, true, false},
	}
	for _, c := range cases {
		st, err := New(c.cfg)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		_, isLocal := st.(*Local)
		if isLocal != c.local {
			t.Errorf("%s: got %T", c.name, st)
		}
	}
}

func TestNewS3Validation(t *testing.T) {
	for _, cfg := range []config.S3{
		{},
		{Endpoint: "e"},
		{Bucket: "b"},
	} {
		if _, err := NewS3(cfg); err == nil {
			t.Errorf("NewS3(%+v) should error", cfg)
		}
	}
	// defaults: empty region filled, insecure scheme accepted
	if _, err := NewS3(config.S3{Endpoint: "e", Bucket: "b", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewS3(config.S3{Endpoint: "e", Bucket: "b", Region: "eu-west-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestNewLocalMkdirError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// root nested under a regular file → MkdirAll fails
	if _, err := NewLocal(filepath.Join(f, "sub")); err == nil {
		t.Fatal("NewLocal under a file should error")
	}
}

func TestLocalErrorPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	// A plain file where a key directory should be.
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Put: MkdirAll fails because a path component is a file.
	if err := l.Put(ctx, "blocker/sub/key", strings.NewReader("x")); err == nil {
		t.Fatal("Put under a file should error")
	}
	// Put: reader error propagates and leaves no temp file behind.
	if err := l.Put(ctx, "chunk/aa/k", iotest.ErrReader(io.ErrUnexpectedEOF)); err == nil {
		t.Fatal("Put with failing reader should error")
	}
	assertNoTmp(t, filepath.Join(root, "chunk", "aa"))

	// Put: CreateTemp fails in an unwritable destination directory.
	roDir := filepath.Join(root, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })
	if os.Getuid() != 0 { // root ignores mode bits
		if err := l.Put(ctx, "ro/key", strings.NewReader("x")); err == nil {
			t.Fatal("Put into read-only dir should error")
		}
	}

	// Put: rename fails when the destination is a non-empty directory.
	if err := os.MkdirAll(filepath.Join(root, "dirkey", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Put(ctx, "dirkey", strings.NewReader("x")); err == nil {
		t.Fatal("Put onto a non-empty directory should error")
	}

	// Has: Stat error that is not ErrNotExist (component is a file → ENOTDIR).
	if _, err := l.Has(ctx, "blocker/sub"); err == nil {
		t.Fatal("Has through a file should error")
	}
	// Delete: same non-ENOENT error surfaces.
	if err := l.Delete(ctx, "blocker/sub"); err == nil {
		t.Fatal("Delete through a file should error")
	}
}

// ---- S3 against a local fake endpoint (no network, no minio) ----

// fakeS3 speaks just enough of the S3 REST dialect for the aws-sdk client:
// path-style /bucket/key with PUT/GET/HEAD/DELETE, plus POST ?delete=
// (multi-object delete).
type fakeS3 struct {
	mu          sync.Mutex
	objs        map[string][]byte
	deleteCalls int // POST ?delete= requests seen
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodPost:
		if _, ok := r.URL.Query()["delete"]; !ok {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		f.deleteCalls++
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Objects []struct {
				Key string `xml:"Key"`
			} `xml:"Object"`
		}
		if err := xml.Unmarshal(b, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var res strings.Builder
		res.WriteString(`<?xml version="1.0"?><DeleteResult>`)
		for _, o := range req.Objects {
			switch {
			case o.Key == "poison":
				res.WriteString(`<Error><Key>poison</Key><Code>AccessDenied</Code><Message>no</Message></Error>`)
			case f.objs[o.Key] == nil:
				// stricter than AWS quiet mode: report missing keys so the
				// client's NoSuchKey tolerance is exercised
				res.WriteString(`<Error><Key>` + o.Key + `</Key><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			default:
				delete(f.objs, o.Key)
			}
		}
		res.WriteString(`</DeleteResult>`)
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, res.String())
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
	case http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			return
		}
		w.Write(b)
	case http.MethodHead:
		if key == "forbidden" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if _, ok := f.objs[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	case http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func TestS3RoundTrip(t *testing.T) {
	srv := httptest.NewServer(&fakeS3{objs: map[string][]byte{}})
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	s, err := NewS3(config.S3{
		Endpoint: u.Host, Bucket: "bucket", Insecure: true,
		AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := ChunkKey("abcd1234")
	data := []byte("hello s3")

	if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}
	if ok, err := s.Has(ctx, key); err != nil || !ok {
		t.Fatalf("Has(existing) = %v, %v", ok, err)
	}
	if ok, err := s.Has(ctx, ChunkKey("ffffffff")); err != nil || ok {
		t.Fatalf("Has(missing) = %v, %v", ok, err)
	}
	// a non-404 error is surfaced, not mapped to "absent"
	if _, err := s.Has(ctx, "forbidden"); err == nil {
		t.Fatal("Has(forbidden) should error")
	}
	if _, err := s.Get(ctx, ChunkKey("ffffffff")); err == nil {
		t.Fatal("Get(missing) should error")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Has(ctx, key); ok {
		t.Fatal("key still present after Delete")
	}
	// idempotent delete of a missing key
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
}

func TestLocalDeleteMany(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{ChunkKey("aa11"), ChunkKey("bb22"), ChunkKey("cc33")}
	for _, k := range keys {
		if err := l.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	// missing key mixed in is fine — Delete is idempotent
	if err := l.DeleteMany(ctx, append(keys, ChunkKey("ffff"))); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if ok, _ := l.Has(ctx, k); ok {
			t.Fatalf("%s still present", k)
		}
	}
	// non-ENOENT error surfaces (path component is a file → ENOTDIR)
	if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteMany(ctx, []string{"blocker/sub"}); err == nil {
		t.Fatal("DeleteMany through a file should error")
	}
}

func TestS3DeleteMany(t *testing.T) {
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	s, err := NewS3(config.S3{
		Endpoint: u.Host, Bucket: "bucket", Insecure: true,
		AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1500 keys cross the 1000-per-call API limit → exactly 2 calls
	keys := make([]string, 1500)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk/aa/%04d", i)
		f.objs[keys[i]] = []byte("x")
	}
	if err := s.DeleteMany(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if f.deleteCalls != 2 {
		t.Fatalf("DeleteObjects calls=%d, want 2", f.deleteCalls)
	}
	if len(f.objs) != 0 {
		t.Fatalf("%d objects remain", len(f.objs))
	}

	// empty slice: no round trip at all
	if err := s.DeleteMany(ctx, nil); err != nil || f.deleteCalls != 2 {
		t.Fatalf("empty DeleteMany: err=%v calls=%d", err, f.deleteCalls)
	}
	// already-deleted keys: per-key NoSuchKey tolerated like Delete
	if err := s.DeleteMany(ctx, keys[:3]); err != nil {
		t.Fatalf("DeleteMany(missing): %v", err)
	}
	// any other per-key error fails the call
	f.objs["ok"] = []byte("x")
	if err := s.DeleteMany(ctx, []string{"ok", "poison"}); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("DeleteMany(poison) = %v, want AccessDenied error", err)
	}
	// transport error surfaces
	srv.Close()
	if err := s.DeleteMany(ctx, []string{"k"}); err == nil {
		t.Fatal("DeleteMany against a dead endpoint should error")
	}
}

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&smithy.GenericAPIError{Code: "NotFound"}, true},
		{&smithy.GenericAPIError{Code: "NoSuchKey"}, true},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{io.ErrUnexpectedEOF, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isNotFound(c.err); got != c.want {
			t.Errorf("isNotFound(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
