package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	opampAddress = "0.0.0.0:4320"
	adminAddress = "0.0.0.0:4321"
)

type controlPlane struct {
	db          *pgxpool.Pool
	logger      *log.Logger
	connections map[types.Connection]*agentConnection
	connMu      sync.RWMutex
}

type agentConnection struct {
	tokenID     int64
	instanceUID []byte
	mu          sync.Mutex
	revoked     bool
}

type configRequest struct {
	Config string `json:"config"`
}

type tokenRequest struct {
	Name string `json:"name"`
}

type configResponse struct {
	Config  string    `json:"config"`
	Hash    string    `json:"hash"`
	Updated time.Time `json:"updated_at"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func runHealthcheck() int {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:4321/healthz")
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	adminUsername := os.Getenv("OPAMP_ADMIN_USERNAME")
	adminPassword := os.Getenv("OPAMP_ADMIN_PASSWORD")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	if adminUsername == "" || len(adminPassword) < 16 {
		return errors.New("OPAMP_ADMIN_USERNAME and an OPAMP_ADMIN_PASSWORD of at least 16 characters are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolConfig.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer pool.Close()

	if err := migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET connected = FALSE`); err != nil {
		return fmt.Errorf("mark agents disconnected before startup: %w", err)
	}

	cp := &controlPlane{
		db:          pool,
		logger:      log.New(os.Stdout, "[control-plane] ", log.LstdFlags|log.LUTC),
		connections: make(map[types.Connection]*agentConnection),
	}

	opampServer := server.New(nil)
	if err := opampServer.Start(server.StartSettings{
		ListenEndpoint: opampAddress,
		Settings: server.Settings{
			MaxMessageSize: 4 << 20,
			Callbacks: types.Callbacks{
				OnConnecting: cp.onConnecting,
			},
		},
	}); err != nil {
		return fmt.Errorf("start OpAMP server: %w", err)
	}

	adminServer := &http.Server{
		Addr:              adminAddress,
		Handler:           cp.adminHandler(adminUsername, adminPassword),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	adminErr := make(chan error, 1)
	go func() {
		cp.logger.Printf("operator UI listening on %s; OpAMP listening on %s", adminAddress, opampAddress)
		if err := adminServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			adminErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-adminErr:
		_ = opampServer.Stop(context.Background())
		return fmt.Errorf("operator HTTP server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adminServer.Shutdown(shutdownCtx); err != nil {
		cp.logger.Printf("operator HTTP shutdown: %v", err)
	}
	if err := opampServer.Stop(shutdownCtx); err != nil {
		cp.logger.Printf("OpAMP shutdown: %v", err)
	}
	return nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	const schema = `
CREATE TABLE IF NOT EXISTS agents (
    instance_uid BYTEA PRIMARY KEY CHECK (octet_length(instance_uid) = 16),
    hostname TEXT NOT NULL DEFAULT '',
    os_type TEXT NOT NULL DEFAULT '',
    agent_type TEXT NOT NULL DEFAULT '',
    agent_version TEXT NOT NULL DEFAULT '',
    source_ip TEXT NOT NULL DEFAULT '',
    agent_description JSONB NOT NULL DEFAULT '{}'::jsonb,
    health JSONB NOT NULL DEFAULT '{}'::jsonb,
    effective_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    reported_config_files JSONB NOT NULL DEFAULT '{}'::jsonb,
    remote_config_status JSONB NOT NULL DEFAULT '{}'::jsonb,
    connected BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS reported_config_files JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS hostname TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS os_type TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS agent_type TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS agent_version TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS source_ip TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS agent_tokens (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    instance_uid BYTEA REFERENCES agents(instance_uid) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS agent_tokens_active_uid_idx
    ON agent_tokens(instance_uid) WHERE revoked_at IS NULL;
CREATE TABLE IF NOT EXISTS config_revisions (
    id BIGSERIAL PRIMARY KEY,
    instance_uid BYTEA NOT NULL REFERENCES agents(instance_uid) ON DELETE CASCADE,
    config TEXT NOT NULL,
    config_hash BYTEA NOT NULL,
    changed_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS agent_configs (
    instance_uid BYTEA PRIMARY KEY REFERENCES agents(instance_uid) ON DELETE CASCADE,
    revision_id BIGINT NOT NULL REFERENCES config_revisions(id),
    config TEXT NOT NULL,
    config_hash BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`
	_, err := pool.Exec(ctx, schema)
	return err
}

func (cp *controlPlane) onConnecting(request *http.Request) types.ConnectionResponse {
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		return rejectedAgentConnection()
	}
	var tokenID int64
	err := cp.db.QueryRow(request.Context(), `
UPDATE agent_tokens SET last_used_at = NOW()
WHERE token_hash = $1 AND revoked_at IS NULL
RETURNING id`, tokenHash(token)).Scan(&tokenID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			cp.logger.Printf("check agent credential: %v", err)
		}
		return rejectedAgentConnection()
	}

	session := &agentConnection{tokenID: tokenID}
	return types.ConnectionResponse{
		Accept: true,
		ConnectionCallbacks: types.ConnectionCallbacks{
			OnConnected: func(_ context.Context, conn types.Connection) {
				cp.connMu.Lock()
				var active bool
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := cp.db.QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM agent_tokens WHERE id = $1 AND revoked_at IS NULL)`,
					tokenID).Scan(&active)
				cancel()
				if err == nil && active {
					cp.connections[conn] = session
				}
				cp.connMu.Unlock()
				if err != nil || !active {
					if err != nil {
						cp.logger.Printf("recheck agent credential: %v", err)
					}
					_ = conn.Disconnect()
				}
			},
			OnMessage: func(ctx context.Context, conn types.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
				return cp.onMessage(ctx, conn, session, message)
			},
			OnConnectionClose: cp.onDisconnect,
		},
	}
}

func rejectedAgentConnection() types.ConnectionResponse {
	return types.ConnectionResponse{Accept: false, HTTPStatusCode: http.StatusUnauthorized}
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return "", false
	}
	return token, true
}

func tokenHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

func newAgentToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate agent credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func (cp *controlPlane) onMessage(ctx context.Context, conn types.Connection, session *agentConnection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
	response := &protobufs.ServerToAgent{
		InstanceUid: message.GetInstanceUid(),
		Capabilities: uint64(
			protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
				protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig |
				protobufs.ServerCapabilities_ServerCapabilities_AcceptsEffectiveConfig,
		),
	}
	if len(message.GetInstanceUid()) != 16 {
		cp.logger.Printf("rejecting unsupported instance UID length %d", len(message.GetInstanceUid()))
		_ = conn.Disconnect()
		return response
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.revoked {
		_ = conn.Disconnect()
		return nil
	}
	cp.connMu.RLock()
	registered := cp.connections[conn] == session
	cp.connMu.RUnlock()
	if !registered {
		_ = conn.Disconnect()
		return nil
	}
	if err := cp.bindTokenToUID(ctx, session.tokenID, message.GetInstanceUid()); err != nil {
		cp.logger.Printf("bind agent credential to %x: %v", message.GetInstanceUid(), err)
		_ = conn.Disconnect()
		return nil
	}
	session.instanceUID = append([]byte(nil), message.GetInstanceUid()...)
	if err := cp.saveStatus(ctx, conn, message); err != nil {
		cp.logger.Printf("persist status for %x: %v", message.GetInstanceUid(), err)
		return response
	}

	config, hash, _, err := cp.desiredConfig(ctx, message.GetInstanceUid())
	if err != nil {
		cp.logger.Printf("load desired config for %x: %v", message.GetInstanceUid(), err)
		return response
	}
	if config != nil {
		response.RemoteConfig = &protobufs.AgentRemoteConfig{
			Config: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigObject{
					"": {Body: []byte(*config)},
				},
			},
			ConfigHash: hash,
		}
	}
	return response
}

func (cp *controlPlane) bindTokenToUID(ctx context.Context, tokenID int64, instanceUID []byte) error {
	tx, err := cp.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin credential binding: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO agents (instance_uid) VALUES ($1) ON CONFLICT (instance_uid) DO NOTHING`,
		instanceUID); err != nil {
		return fmt.Errorf("ensure agent before credential binding: %w", err)
	}
	var boundID int64
	err = tx.QueryRow(ctx, `
UPDATE agent_tokens SET instance_uid = COALESCE(instance_uid, $2)
WHERE id = $1 AND revoked_at IS NULL
  AND (instance_uid IS NULL OR instance_uid = $2)
RETURNING id`, tokenID, instanceUID).Scan(&boundID)
	if err != nil {
		return fmt.Errorf("credential is revoked or bound to another instance: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit credential binding: %w", err)
	}
	return nil
}

func (cp *controlPlane) saveStatus(ctx context.Context, conn types.Connection, message *protobufs.AgentToServer) error {
	description, err := marshalOptional(message.GetAgentDescription())
	if err != nil {
		return fmt.Errorf("marshal agent description: %w", err)
	}
	health, err := marshalOptional(message.GetHealth())
	if err != nil {
		return fmt.Errorf("marshal health: %w", err)
	}
	effectiveConfig, err := marshalOptional(message.GetEffectiveConfig())
	if err != nil {
		return fmt.Errorf("marshal effective config: %w", err)
	}
	configStatus, err := marshalOptional(message.GetRemoteConfigStatus())
	if err != nil {
		return fmt.Errorf("marshal remote config status: %w", err)
	}
	reportedConfigFiles, err := marshalReportedConfigFiles(message.GetEffectiveConfig())
	if err != nil {
		return fmt.Errorf("marshal reported config files: %w", err)
	}
	hostname, osType, agentType, agentVersion := agentMetadata(message.GetAgentDescription())
	sourceIP := connectionSourceIP(conn)
	_, err = cp.db.Exec(ctx, `
INSERT INTO agents (
    instance_uid, hostname, os_type, agent_type, agent_version, source_ip,
    agent_description, health, effective_config, reported_config_files, remote_config_status, connected, last_seen
) VALUES ($1, COALESCE($2, ''), COALESCE($3, ''), COALESCE($4, ''), COALESCE($5, ''), COALESCE($6, ''),
          COALESCE($7::jsonb, '{}'::jsonb), COALESCE($8::jsonb, '{}'::jsonb),
          COALESCE($9::jsonb, '{}'::jsonb), COALESCE($10::jsonb, '{}'::jsonb),
          COALESCE($11::jsonb, '{}'::jsonb), TRUE, NOW())
ON CONFLICT (instance_uid) DO UPDATE SET
    hostname = COALESCE(NULLIF($2, ''), agents.hostname),
    os_type = COALESCE(NULLIF($3, ''), agents.os_type),
    agent_type = COALESCE(NULLIF($4, ''), agents.agent_type),
    agent_version = COALESCE(NULLIF($5, ''), agents.agent_version),
    source_ip = COALESCE(NULLIF($6, ''), agents.source_ip),
    agent_description = COALESCE($7::jsonb, agents.agent_description),
    health = COALESCE($8::jsonb, agents.health),
    effective_config = COALESCE($9::jsonb, agents.effective_config),
    reported_config_files = COALESCE($10::jsonb, agents.reported_config_files),
    remote_config_status = COALESCE($11::jsonb, agents.remote_config_status),
    connected = TRUE,
    last_seen = NOW()`,
		message.GetInstanceUid(), nullableString(hostname), nullableString(osType),
		nullableString(agentType), nullableString(agentVersion), nullableString(sourceIP),
		description, health, effectiveConfig, reportedConfigFiles, configStatus)
	return err
}

func agentMetadata(description *protobufs.AgentDescription) (hostname, osType, agentType, version string) {
	if description == nil {
		return "", "", "", ""
	}
	for _, attribute := range description.GetIdentifyingAttributes() {
		if attribute == nil || attribute.GetValue() == nil {
			continue
		}
		value := attribute.GetValue().GetStringValue()
		switch attribute.GetKey() {
		case "host.name", "host.hostname":
			if hostname == "" {
				hostname = value
			}
		case "os.type":
			osType = value
		case "service.name":
			agentType = value
		case "service.version":
			version = value
		}
	}
	return hostname, osType, agentType, version
}

func connectionSourceIP(conn types.Connection) string {
	if conn == nil || conn.Connection() == nil || conn.Connection().RemoteAddr() == nil {
		return ""
	}
	return sourceIPFromAddr(conn.Connection().RemoteAddr())
}

func sourceIPFromAddr(address net.Addr) string {
	if address == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return ""
	}
	return host
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func marshalReportedConfigFiles(effectiveConfig *protobufs.EffectiveConfig) (*string, error) {
	if effectiveConfig == nil || !effectiveConfig.ProtoReflect().IsValid() {
		return nil, nil
	}
	files := make(map[string]string)
	if effectiveConfig.GetConfigMap() != nil {
		for name, config := range effectiveConfig.GetConfigMap().GetConfigMap() {
			if config != nil {
				files[name] = string(config.GetBody())
			}
		}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	result := string(encoded)
	return &result, nil
}

func marshalOptional(message proto.Message) (*string, error) {
	if message == nil || !message.ProtoReflect().IsValid() {
		return nil, nil
	}
	encoded, err := protojson.Marshal(message)
	if err != nil {
		return nil, err
	}
	result := string(encoded)
	return &result, nil
}

func (cp *controlPlane) onDisconnect(conn types.Connection) {
	cp.connMu.Lock()
	session, connected := cp.connections[conn]
	delete(cp.connections, conn)
	cp.connMu.Unlock()
	if !connected {
		return
	}
	session.mu.Lock()
	instanceUID := append([]byte(nil), session.instanceUID...)
	session.mu.Unlock()
	if len(instanceUID) != 16 {
		return
	}
	cp.markDisconnectedIfNoConnection(instanceUID)
}

func (cp *controlPlane) markDisconnectedIfNoConnection(instanceUID []byte) {
	cp.connMu.RLock()
	sessions := make([]*agentConnection, 0, len(cp.connections))
	for _, session := range cp.connections {
		sessions = append(sessions, session)
	}
	cp.connMu.RUnlock()
	for _, session := range sessions {
		session.mu.Lock()
		active := !session.revoked && string(session.instanceUID) == string(instanceUID)
		session.mu.Unlock()
		if active {
			return
		}
	}
	if _, err := cp.db.Exec(context.Background(),
		`UPDATE agents SET connected = FALSE WHERE instance_uid = $1`, instanceUID); err != nil {
		cp.logger.Printf("mark agent %x disconnected: %v", instanceUID, err)
	}
}

func (cp *controlPlane) desiredConfig(ctx context.Context, instanceUID []byte) (*string, []byte, time.Time, error) {
	var config string
	var hash []byte
	var updated time.Time
	err := cp.db.QueryRow(ctx,
		`SELECT config, config_hash, updated_at FROM agent_configs WHERE instance_uid = $1`,
		instanceUID).Scan(&config, &hash, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, time.Time{}, nil
	}
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return &config, hash, updated, nil
}

func (cp *controlPlane) adminHandler(username, password string) http.Handler {
	mux := http.NewServeMux()
	assets, err := adminUIAssetsHandler()
	if err != nil {
		cp.logger.Printf("load admin UI assets: %v", err)
		assets = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "admin UI assets are unavailable", http.StatusInternalServerError)
		})
	}
	healthHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := cp.db.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /assets/", assets)
	mux.HandleFunc("GET /", serveAdminUI)
	mux.HandleFunc("GET /api/v1/agents", cp.listAgents)
	mux.HandleFunc("PUT /api/v1/agents/{uid}/config", cp.putConfig)
	mux.HandleFunc("GET /api/v1/agents/{uid}/config", cp.getConfig)
	mux.HandleFunc("POST /api/v1/agents/{uid}/token", cp.createAgentToken)
	mux.HandleFunc("GET /api/v1/agent-tokens", cp.listAgentTokens)
	mux.HandleFunc("POST /api/v1/agent-tokens", cp.createBootstrapToken)
	mux.HandleFunc("POST /api/v1/agent-tokens/{id}/rotate", cp.rotateAgentToken)
	mux.HandleFunc("DELETE /api/v1/agent-tokens/{id}", cp.revokeAgentToken)
	protected := basicAuth(username, password, mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			healthHandler.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func basicAuth(username, password string, next http.Handler) http.Handler {
	expectedUser := sha256.Sum256([]byte(username))
	expectedPassword := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		if ok {
			userHash := sha256.Sum256([]byte(gotUser))
			passwordHash := sha256.Sum256([]byte(gotPassword))
			ok = subtle.ConstantTimeCompare(userHash[:], expectedUser[:]) == 1 &&
				subtle.ConstantTimeCompare(passwordHash[:], expectedPassword[:]) == 1
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="OpAMP control plane", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (cp *controlPlane) listAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := cp.db.Query(r.Context(), `
SELECT encode(a.instance_uid, 'hex'), a.agent_description, a.health, a.effective_config,
       a.reported_config_files, a.remote_config_status, a.connected, a.last_seen,
       COALESCE(c.config, ''), a.hostname, a.os_type, a.agent_type, a.agent_version, a.source_ip
FROM agents a
LEFT JOIN agent_configs c ON c.instance_uid = a.instance_uid
ORDER BY a.last_seen DESC`)
	if err != nil {
		http.Error(w, "could not list agents", http.StatusInternalServerError)
		cp.logger.Printf("list agents: %v", err)
		return
	}
	defer rows.Close()

	type agent struct {
		InstanceUID         string          `json:"instance_uid"`
		AgentDescription    json.RawMessage `json:"agent_description"`
		Health              json.RawMessage `json:"health"`
		EffectiveConfig     json.RawMessage `json:"effective_config"`
		ReportedConfigFiles json.RawMessage `json:"reported_config_files"`
		RemoteConfigStatus  json.RawMessage `json:"remote_config_status"`
		Connected           bool            `json:"connected"`
		LastSeen            time.Time       `json:"last_seen"`
		DesiredConfig       string          `json:"desired_config"`
		Hostname            string          `json:"hostname"`
		OSType              string          `json:"os_type"`
		AgentType           string          `json:"agent_type"`
		AgentVersion        string          `json:"agent_version"`
		SourceIP            string          `json:"source_ip"`
	}
	agents := make([]agent, 0)
	for rows.Next() {
		var item agent
		if err := rows.Scan(&item.InstanceUID, &item.AgentDescription, &item.Health, &item.EffectiveConfig, &item.ReportedConfigFiles, &item.RemoteConfigStatus, &item.Connected, &item.LastSeen, &item.DesiredConfig, &item.Hostname, &item.OSType, &item.AgentType, &item.AgentVersion, &item.SourceIP); err != nil {
			http.Error(w, "could not read agent records", http.StatusInternalServerError)
			cp.logger.Printf("scan agent record: %v", err)
			return
		}
		agents = append(agents, item)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "could not read agent records", http.StatusInternalServerError)
		cp.logger.Printf("iterate agent records: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

type agentTokenResponse struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Token       string    `json:"token,omitempty"`
	InstanceUID string    `json:"instance_uid,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	LastUsedAt  string    `json:"last_used_at,omitempty"`
	RevokedAt   string    `json:"revoked_at,omitempty"`
}

func (cp *controlPlane) listAgentTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := cp.db.Query(r.Context(), `
SELECT id, name, COALESCE(encode(instance_uid, 'hex'), ''),
       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
       COALESCE(to_char(last_used_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''),
       COALESCE(to_char(revoked_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '')
FROM agent_tokens
ORDER BY created_at DESC, id DESC`)
	if err != nil {
		http.Error(w, "could not list agent credentials", http.StatusInternalServerError)
		cp.logger.Printf("list agent credentials: %v", err)
		return
	}
	defer rows.Close()
	tokens := make([]agentTokenResponse, 0)
	for rows.Next() {
		var token agentTokenResponse
		var createdAt string
		if err := rows.Scan(&token.ID, &token.Name, &token.InstanceUID, &createdAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
			http.Error(w, "could not read agent credentials", http.StatusInternalServerError)
			cp.logger.Printf("scan agent credential: %v", err)
			return
		}
		token.CreatedAt, err = time.Parse("2006-01-02T15:04:05.999999Z", createdAt)
		if err != nil {
			http.Error(w, "could not read agent credentials", http.StatusInternalServerError)
			cp.logger.Printf("parse agent credential timestamp: %v", err)
			return
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "could not read agent credentials", http.StatusInternalServerError)
		cp.logger.Printf("iterate agent credentials: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func decodeTokenRequest(w http.ResponseWriter, r *http.Request) (tokenRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input tokenRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return input, errors.New("request must contain exactly one JSON object")
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		return input, errors.New("credential name must be between 1 and 100 characters")
	}
	return input, nil
}

func (cp *controlPlane) createBootstrapToken(w http.ResponseWriter, r *http.Request) {
	input, err := decodeTokenRequest(w, r)
	if err != nil {
		http.Error(w, "invalid credential request: "+err.Error(), http.StatusBadRequest)
		return
	}
	cp.createTokenResponse(w, r, input.Name, nil)
}

func (cp *controlPlane) createAgentToken(w http.ResponseWriter, r *http.Request) {
	instanceUID, err := decodeUID(r.PathValue("uid"))
	if err != nil {
		http.Error(w, "instance UID must be 32 hexadecimal characters", http.StatusBadRequest)
		return
	}
	input, err := decodeTokenRequest(w, r)
	if err != nil {
		http.Error(w, "invalid credential request: "+err.Error(), http.StatusBadRequest)
		return
	}
	cp.createTokenResponse(w, r, input.Name, instanceUID)
}

func (cp *controlPlane) createTokenResponse(w http.ResponseWriter, r *http.Request, name string, instanceUID []byte) {
	token, err := newAgentToken()
	if err != nil {
		http.Error(w, "could not create agent credential", http.StatusInternalServerError)
		cp.logger.Printf("generate agent credential: %v", err)
		return
	}
	ctx := r.Context()
	tx, err := cp.db.Begin(ctx)
	if err != nil {
		http.Error(w, "could not create agent credential", http.StatusInternalServerError)
		cp.logger.Printf("begin credential transaction: %v", err)
		return
	}
	defer tx.Rollback(ctx)
	if instanceUID != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (instance_uid) VALUES ($1) ON CONFLICT (instance_uid) DO NOTHING`, instanceUID); err != nil {
			http.Error(w, "could not create agent credential", http.StatusInternalServerError)
			cp.logger.Printf("ensure agent for credential: %v", err)
			return
		}
	}
	var result agentTokenResponse
	result.Name = name
	result.Token = token
	if instanceUID != nil {
		result.InstanceUID = hex.EncodeToString(instanceUID)
	}
	err = tx.QueryRow(ctx, `
INSERT INTO agent_tokens (name, token_hash, instance_uid)
VALUES ($1, $2, $3)
RETURNING id, created_at`, name, tokenHash(token), instanceUID).Scan(&result.ID, &result.CreatedAt)
	if err != nil {
		http.Error(w, "could not create agent credential", http.StatusInternalServerError)
		cp.logger.Printf("insert agent credential: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not create agent credential", http.StatusInternalServerError)
		cp.logger.Printf("commit agent credential: %v", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, result)
}

func parseTokenID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid credential ID")
	}
	return id, nil
}

func (cp *controlPlane) rotateAgentToken(w http.ResponseWriter, r *http.Request) {
	tokenID, err := parseTokenID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "credential ID must be a positive integer", http.StatusBadRequest)
		return
	}
	token, err := newAgentToken()
	if err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("generate rotated credential: %v", err)
		return
	}
	ctx := r.Context()
	tx, err := cp.db.Begin(ctx)
	if err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("begin credential rotation: %v", err)
		return
	}
	defer tx.Rollback(ctx)
	var result agentTokenResponse
	var instanceUID []byte
	err = tx.QueryRow(ctx, `
SELECT name, instance_uid FROM agent_tokens
WHERE id = $1 AND revoked_at IS NULL FOR UPDATE`, tokenID).Scan(&result.Name, &instanceUID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "active credential not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("read credential for rotation: %v", err)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_tokens SET revoked_at = NOW() WHERE id = $1`, tokenID); err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("revoke rotated credential: %v", err)
		return
	}
	if instanceUID != nil {
		result.InstanceUID = hex.EncodeToString(instanceUID)
	}
	result.Token = token
	err = tx.QueryRow(ctx, `
INSERT INTO agent_tokens (name, token_hash, instance_uid)
VALUES ($1, $2, $3)
RETURNING id, created_at`, result.Name, tokenHash(token), instanceUID).Scan(&result.ID, &result.CreatedAt)
	if err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("insert rotated credential: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not rotate agent credential", http.StatusInternalServerError)
		cp.logger.Printf("commit credential rotation: %v", err)
		return
	}
	cp.disconnectCredential(tokenID, instanceUID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, result)
}

func (cp *controlPlane) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	tokenID, err := parseTokenID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "credential ID must be a positive integer", http.StatusBadRequest)
		return
	}
	var instanceUID []byte
	err = cp.db.QueryRow(r.Context(), `
UPDATE agent_tokens SET revoked_at = NOW()
WHERE id = $1 AND revoked_at IS NULL
RETURNING instance_uid`, tokenID).Scan(&instanceUID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "active credential not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not revoke agent credential", http.StatusInternalServerError)
		cp.logger.Printf("revoke agent credential: %v", err)
		return
	}
	cp.disconnectCredential(tokenID, instanceUID)
	w.WriteHeader(http.StatusNoContent)
}

func (cp *controlPlane) disconnectCredential(tokenID int64, instanceUID []byte) {
	cp.connMu.Lock()
	type revokedConnection struct {
		conn    types.Connection
		session *agentConnection
	}
	var connections []revokedConnection
	for conn, session := range cp.connections {
		if session.tokenID == tokenID {
			delete(cp.connections, conn)
			connections = append(connections, revokedConnection{conn: conn, session: session})
		}
	}
	cp.connMu.Unlock()
	for _, connection := range connections {
		connection.session.mu.Lock()
		connection.session.revoked = true
		connection.session.mu.Unlock()
		_ = connection.conn.Disconnect()
	}
	if len(instanceUID) == 16 {
		cp.markDisconnectedIfNoConnection(instanceUID)
	}
}

func (cp *controlPlane) putConfig(w http.ResponseWriter, r *http.Request) {
	instanceUID, err := decodeUID(r.PathValue("uid"))
	if err != nil {
		http.Error(w, "instance UID must be 32 hexadecimal characters", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var input configRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid JSON config request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "request must contain exactly one JSON object", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tx, err := cp.db.Begin(ctx)
	if err != nil {
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
		cp.logger.Printf("begin config transaction: %v", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
INSERT INTO agents (instance_uid) VALUES ($1)
ON CONFLICT (instance_uid) DO NOTHING`, instanceUID); err != nil {
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
		cp.logger.Printf("ensure agent before config save: %v", err)
		return
	}
	hash := sha256.Sum256([]byte(input.Config))
	var revisionID int64
	operator, _, _ := r.BasicAuth()
	if err := tx.QueryRow(ctx, `
INSERT INTO config_revisions (instance_uid, config, config_hash, changed_by)
VALUES ($1, $2, $3, $4) RETURNING id`,
		instanceUID, input.Config, hash[:], operator).Scan(&revisionID); err != nil {
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
		cp.logger.Printf("insert config revision: %v", err)
		return
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO agent_configs (instance_uid, revision_id, config, config_hash, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (instance_uid) DO UPDATE SET
    revision_id = EXCLUDED.revision_id,
    config = EXCLUDED.config,
    config_hash = EXCLUDED.config_hash,
    updated_at = NOW()`, instanceUID, revisionID, input.Config, hash[:]); err != nil {
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
		cp.logger.Printf("upsert desired config: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
		cp.logger.Printf("commit config transaction: %v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"instance_uid": hex.EncodeToString(instanceUID),
		"hash":         hex.EncodeToString(hash[:]),
		"message":      "saved; offered when the agent reports status",
	})
}

func (cp *controlPlane) getConfig(w http.ResponseWriter, r *http.Request) {
	instanceUID, err := decodeUID(r.PathValue("uid"))
	if err != nil {
		http.Error(w, "instance UID must be 32 hexadecimal characters", http.StatusBadRequest)
		return
	}
	config, hash, updated, err := cp.desiredConfig(r.Context(), instanceUID)
	if errors.Is(err, pgx.ErrNoRows) || config == nil {
		http.Error(w, "no desired config for agent", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "could not read configuration", http.StatusInternalServerError)
		cp.logger.Printf("read desired config: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, configResponse{
		Config: *config, Hash: hex.EncodeToString(hash), Updated: updated,
	})
}

func decodeUID(value string) ([]byte, error) {
	if len(value) != 32 {
		return nil, errors.New("invalid UID length")
	}
	uid, err := hex.DecodeString(value)
	if err != nil || len(uid) != 16 {
		return nil, errors.New("invalid UID")
	}
	return uid, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}
