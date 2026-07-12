package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAPIClientDo(t *testing.T) {
	var gotAuth, gotMethod, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(buf)
			gotBody = string(buf)
		}
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(`{"value":"hi"}`))
		case "/created":
			w.WriteHeader(http.StatusCreated)
		case "/jsonerr":
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"boom"}`))
		case "/texterr":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("plain denial"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL, "sekret")

	// Success with body decode + request marshaling.
	var out struct {
		Value string `json:"value"`
	}
	if err := c.do(http.MethodPost, "/ok", map[string]string{"k": "v"}, &out); err != nil {
		t.Fatalf("do ok: %v", err)
	}
	if out.Value != "hi" {
		t.Fatalf("decoded %+v", out)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotMethod != http.MethodPost || gotBody != `{"k":"v"}` {
		t.Fatalf("request = %s %q", gotMethod, gotBody)
	}

	// nil in (no body) and nil out (ignore body).
	if err := c.do(http.MethodPost, "/created", nil, nil); err != nil {
		t.Fatalf("do created: %v", err)
	}

	// JSON error body surfaces the server message.
	err := c.do(http.MethodGet, "/jsonerr", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("json error = %v", err)
	}
	// Non-JSON error body falls back to the trimmed raw text.
	err = c.do(http.MethodGet, "/texterr", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "plain denial") {
		t.Fatalf("text error = %v", err)
	}
}

func TestDefaultCacheFromEnv(t *testing.T) {
	t.Setenv("XILO_CACHE", "acme/prod")
	if got := defaultCache(); got != "acme/prod" {
		t.Fatalf("defaultCache = %q", got)
	}
	os.Unsetenv("XILO_CACHE")
	// Without env and without a saved profile it resolves to "" — just must
	// not panic reading the (absent) client config.
	_ = defaultCache()
}

func TestIsTTY(t *testing.T) {
	// Under `go test` stdout is a pipe, not a terminal; the call must not panic.
	if isTTY() {
		t.Log("stdout is a terminal in this environment")
	}
}
