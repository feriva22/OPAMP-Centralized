package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
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

func TestMarshalReportedConfigFiles(t *testing.T) {
	config, err := marshalReportedConfigFiles(&protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{
				"":       {Body: []byte("receivers:\n  otlp:\n")},
				"extra":  {Body: []byte("processors:\n  batch:\n")},
				"absent": nil,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshalReportedConfigFiles(): %v", err)
	}
	if config == nil {
		t.Fatal("expected reported config files")
	}

	var files map[string]string
	if err := json.Unmarshal([]byte(*config), &files); err != nil {
		t.Fatalf("unmarshal config files: %v", err)
	}
	if got, want := files[""], "receivers:\n  otlp:\n"; got != want {
		t.Errorf("unnamed config = %q, want %q", got, want)
	}
	if got, want := files["extra"], "processors:\n  batch:\n"; got != want {
		t.Errorf("named config = %q, want %q", got, want)
	}
	if _, ok := files["absent"]; ok {
		t.Error("nil config object should be omitted")
	}
}

func TestMarshalAbsentStatusDoesNotOverwriteExistingValues(t *testing.T) {
	description, err := marshalOptional((*protobufs.AgentDescription)(nil))
	if err != nil {
		t.Fatalf("marshalOptional(): %v", err)
	}
	if description != nil {
		t.Fatalf("marshalOptional(nil) = %q, want nil to preserve prior report", *description)
	}

	reported, err := marshalReportedConfigFiles(nil)
	if err != nil {
		t.Fatalf("marshalReportedConfigFiles(nil): %v", err)
	}
	if reported != nil {
		t.Fatalf("marshalReportedConfigFiles(nil) = %q, want nil to preserve prior report", *reported)
	}
}
