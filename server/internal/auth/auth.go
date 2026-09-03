// Package auth implements the bearer-token scheme in PRD.md §8: a random
// token generated on first run, stored 0600, read by clients and hooks.
// The token is never logged, never returned in a response body, and never
// exposed via /state or /health.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// tokenBytes is the entropy of a generated token. There is no rotation
// or expiry — this is a personal LAN tool, not a multi-tenant service.
const tokenBytes = 32

// LoadOrCreate reads the token at path, generating and persisting one on
// first run. The file is written 0600 inside a 0700 directory.
func LoadOrCreate(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty; delete it to regenerate", path)
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token file: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token directory: %w", err)
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token file: %w", err)
	}
	return token, nil
}

// Require wraps next with bearer-token authentication. Failures return a
// bare 401 — no hint about the expected value, and nothing echoed back.
func Require(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearer(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(presented), expected) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(header string) (string, bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}
