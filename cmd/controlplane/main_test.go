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

func TestAdminUIProvidesLoginPage(t *testing.T) {
	handler := (&controlPlane{}).adminHandler("operator", "a-long-staging-password")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("login page status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want HTML", got)
	}
	assetPath := regexp.MustCompile(`src="/assets/([^"]+\.js)"`).FindStringSubmatch(response.Body.String())
	if len(assetPath) != 2 {
		t.Fatal("embedded admin page does not reference a built JavaScript asset")
	}
	assetRequest := httptest.NewRequest(http.MethodGet, "/assets/"+assetPath[1], nil)
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want %d", assetResponse.Code, http.StatusOK)
	}
	if assetResponse.Body.Len() == 0 {
		t.Fatal("embedded JavaScript asset is empty")
	}
}

func TestAdminLoginAndSessionCookie(t *testing.T) {
	const username = "operator"
	const password = "a-long-staging-password"
	handler := (&controlPlane{}).adminHandler(username, password)

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"authenticated":false`) {
		t.Fatalf("unauthenticated session response = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	protectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(protectedResponse, httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil))
	if protectedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d, want %d", protectedResponse.Code, http.StatusUnauthorized)
	}

	wrongRequest := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"operator","password":"wrong-password"}`))
	wrongRequest.Header.Set("Content-Type", "application/json")
	wrongResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongResponse, wrongRequest)
	if wrongResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login status = %d, want %d", wrongResponse.Code, http.StatusUnauthorized)
	}

	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"operator","password":"a-long-staging-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("X-Forwarded-Proto", "https")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d", loginResponse.Code, http.StatusOK)
	}
	cookies := loginResponse.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminSessionCookie {
		t.Fatalf("login cookies = %#v, want one admin session cookie", cookies)
	}
	if !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie security attributes are incomplete: %#v", cookies[0])
	}

	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	sessionRequest.AddCookie(cookies[0])
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK || !strings.Contains(sessionResponse.Body.String(), `"authenticated":true`) {
		t.Fatalf("authenticated session response = %d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	if !validAdminSession(sessionRequest, username, password) {
		t.Fatal("valid login cookie was rejected")
	}
	authenticatedRequest := httptest.NewRequest(http.MethodGet, "/private-route-not-found", nil)
	authenticatedRequest.AddCookie(cookies[0])
	authenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(authenticatedResponse, authenticatedRequest)
	if authenticatedResponse.Code != http.StatusNotFound {
		t.Fatalf("authenticated request status = %d, want route's %d response", authenticatedResponse.Code, http.StatusNotFound)
	}

	tamperedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	tamperedRequest.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: cookies[0].Value + "tampered"})
	if validAdminSession(tamperedRequest, username, password) {
		t.Fatal("tampered session cookie was accepted")
	}

	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil))
	logoutCookies := logoutResponse.Result().Cookies()
	if len(logoutCookies) != 1 || logoutCookies[0].MaxAge >= 0 {
		t.Fatalf("logout cookie = %#v, want expired cookie", logoutCookies)
	}
	logoutRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	logoutRequest.AddCookie(logoutCookies[0])
	if validAdminSession(logoutRequest, username, password) {
		t.Fatal("logged-out cookie was accepted")
	}
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
