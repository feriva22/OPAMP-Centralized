package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuth(t *testing.T) {
	handler := basicAuth("operator", "a-long-staging-password", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	t.Run("rejects missing credentials", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
		if response.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("missing WWW-Authenticate challenge")
		}
	})

	t.Run("accepts correct credentials", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.SetBasicAuth("operator", "a-long-staging-password")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
		}
	})

	t.Run("rejects incorrect password", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.SetBasicAuth("operator", "wrong-password")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
		}
	})
}

func TestDecodeUID(t *testing.T) {
	const valid = "0123456789abcdef0123456789abcdef"
	uid, err := decodeUID(valid)
	if err != nil {
		t.Fatalf("decodeUID(%q): %v", valid, err)
	}
	if len(uid) != 16 {
		t.Fatalf("decoded UID length = %d, want 16", len(uid))
	}

	for _, invalid := range []string{"", "0123", "0123456789abcdef0123456789abcdeg"} {
		if _, err := decodeUID(invalid); err == nil {
			t.Errorf("decodeUID(%q) unexpectedly succeeded", invalid)
		}
	}
}
