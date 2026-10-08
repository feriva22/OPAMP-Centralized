package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestAdminUIIsEmbeddedAndProtected(t *testing.T) {
	handler := (&controlPlane{}).adminHandler("operator", "a-long-staging-password")

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("operator", "a-long-staging-password")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want HTML", got)
	}
	assetPath := regexp.MustCompile(`src="/assets/([^"]+\.js)"`).FindStringSubmatch(response.Body.String())
	if len(assetPath) != 2 {
		t.Fatal("embedded admin page does not reference a built JavaScript asset")
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/assets/"+assetPath[1], nil)
	assetRequest.SetBasicAuth("operator", "a-long-staging-password")
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want %d", assetResponse.Code, http.StatusOK)
	}
	if assetResponse.Body.Len() == 0 {
		t.Fatal("embedded JavaScript asset is empty")
	}
}

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

func TestBearerToken(t *testing.T) {
	for _, test := range []struct {
		header string
		want   string
		ok     bool
	}{
		{header: "Bearer " + strings.Repeat("a", 43), want: strings.Repeat("a", 43), ok: true},
		{header: "Basic abc", ok: false},
		{header: "Bearer ", ok: false},
		{header: "Bearer token with-space", ok: false},
		{header: "Bearer\ttoken", ok: false},
		{header: "Bearer too-short", ok: false},
	} {
		t.Run(test.header, func(t *testing.T) {
			got, ok := bearerToken(test.header)
			if got != test.want || ok != test.ok {
				t.Fatalf("bearerToken(%q) = (%q, %t), want (%q, %t)", test.header, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestNewAgentToken(t *testing.T) {
	token, err := newAgentToken()
	if err != nil {
		t.Fatalf("newAgentToken(): %v", err)
	}
	if len(token) != 43 || strings.ContainsAny(token, "+/=") {
		t.Fatalf("newAgentToken() = %q, want 43-character URL-safe token", token)
	}
	if got := tokenHash(token); len(got) != 32 {
		t.Fatalf("tokenHash() length = %d, want 32", len(got))
	}
	if string(tokenHash(token)) == token {
		t.Fatal("tokenHash() unexpectedly returned plaintext token")
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

func TestAgentMetadata(t *testing.T) {
	description := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "host.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "vm-01"}}},
			{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "otelcol-contrib"}}},
			{Key: "service.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "0.159.0"}}},
		},
		NonIdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "os.type", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "linux"}}},
			{Key: "os.description", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "Ubuntu 26.04"}}},
		},
	}

	hostname, osType, osDescription, agentType, version := agentMetadata(description)
	if hostname != "vm-01" || osType != "linux" || osDescription != "Ubuntu 26.04" || agentType != "otelcol-contrib" || version != "0.159.0" {
		t.Fatalf("agentMetadata() = (%q, %q, %q, %q, %q)", hostname, osType, osDescription, agentType, version)
	}
}

func TestStoredAgentMetadataReadsNonIdentifyingAttributes(t *testing.T) {
	description := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "service.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "0.159.0"}}},
		},
		NonIdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "os.type", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "linux"}}},
			{Key: "os.description", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "Ubuntu 26.04"}}},
		},
	}
	encoded, err := protojson.Marshal(description)
	if err != nil {
		t.Fatalf("marshal agent description: %v", err)
	}

	hostname, osType, osDescription, _, version, err := storedAgentMetadata(encoded)
	if err != nil {
		t.Fatalf("storedAgentMetadata(): %v", err)
	}
	if hostname != "" || osType != "linux" || osDescription != "Ubuntu 26.04" || version != "0.159.0" {
		t.Fatalf("storedAgentMetadata() = (%q, %q, %q, %q), want empty hostname, linux, Ubuntu 26.04, 0.159.0", hostname, osType, osDescription, version)
	}
}

func TestSourceIPFromAddr(t *testing.T) {
	for _, test := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{name: "ipv4", addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.15"), Port: 4320}, want: "192.0.2.15"},
		{name: "ipv6", addr: &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4320}, want: "2001:db8::1"},
		{name: "nil", addr: nil, want: ""},
		{name: "invalid", addr: testAddr("unknown"), want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sourceIPFromAddr(test.addr); got != test.want {
				t.Fatalf("sourceIPFromAddr() = %q, want %q", got, test.want)
			}
		})
	}
}

type testAddr string

func (addr testAddr) Network() string { return "test" }
func (addr testAddr) String() string  { return string(addr) }
