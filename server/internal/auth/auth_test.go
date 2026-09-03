package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadOrCreateGeneratesOnFirstRun checks generation and, critically,
// the 0600 file / 0700 directory permissions required by PRD.md §8.
func TestLoadOrCreateGeneratesOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "token")

	token, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != tokenBytes*2 {
		t.Fatalf("token is %d chars, want %d hex chars", len(token), tokenBytes*2)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("token directory mode = %o, want 700", perm)
	}
}

// TestLoadOrCreateIsStableAcrossRestarts: a restart must not invalidate
// the token every client and hook already has.
func TestLoadOrCreateIsStableAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("token regenerated on second load")
	}
}

// TestTokensAreDistinct guards against a fixed or predictable default.
func TestTokensAreDistinct(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 20; i++ {
		token, err := LoadOrCreate(filepath.Join(t.TempDir(), "token"))
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[token]; dup {
			t.Fatal("generated a duplicate token")
		}
		seen[token] = struct{}{}
	}
}

func TestTrailingNewlineIsTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  secret\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret" {
		t.Fatalf("token = %q, want %q", token, "secret")
	}
}

func TestEmptyTokenFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("an empty token file was accepted; every request would then be unauthenticated")
	}
}

func TestRequire(t *testing.T) {
	const token = "s3cret"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := Require(token, next)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid", "Bearer s3cret", http.StatusNoContent},
		{"scheme is case-insensitive", "bearer s3cret", http.StatusNoContent},
		{"extra whitespace", "Bearer   s3cret  ", http.StatusNoContent},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"no scheme", "s3cret", http.StatusUnauthorized},
		{"wrong scheme", "Basic s3cret", http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"prefix of the real token", "Bearer s3cre", http.StatusUnauthorized},
		{"token plus suffix", "Bearer s3crets", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/state", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
		})
	}
}

// TestUnauthorizedResponseDoesNotLeakTheToken: the token is never echoed
// back in any response (PRD.md §8).
func TestUnauthorizedResponseDoesNotLeakTheToken(t *testing.T) {
	const token = "very-secret-token"
	handler := Require(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/state", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if strings.Contains(rr.Body.String(), token) {
		t.Fatal("response body contains the token")
	}
	for key, values := range rr.Header() {
		for _, v := range values {
			if strings.Contains(v, token) {
				t.Fatalf("header %s contains the token", key)
			}
		}
	}
}
