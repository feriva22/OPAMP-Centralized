package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	connections map[types.Connection][]byte
	connMu      sync.RWMutex
}

type configRequest struct {
	Config string `json:"config"`
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
		connections: make(map[types.Connection][]byte),
	}

	opampServer := server.New(nil)
	if err := opampServer.Start(server.StartSettings{
		ListenEndpoint: opampAddress,
		Settings: server.Settings{
			MaxMessageSize: 4 << 20,
			Callbacks: types.Callbacks{
				OnConnecting: func(_ *http.Request) types.ConnectionResponse {
					return types.ConnectionResponse{
						Accept: true,
						ConnectionCallbacks: types.ConnectionCallbacks{
							OnMessage:         cp.onMessage,
							OnConnectionClose: cp.onDisconnect,
						},
					}
				},
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
    agent_description JSONB NOT NULL DEFAULT '{}'::jsonb,
    health JSONB NOT NULL DEFAULT '{}'::jsonb,
    effective_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    remote_config_status JSONB NOT NULL DEFAULT '{}'::jsonb,
    connected BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
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

func (cp *controlPlane) onMessage(ctx context.Context, conn types.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
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
		return response
	}

	if err := cp.saveStatus(ctx, message); err != nil {
		cp.logger.Printf("persist status for %x: %v", message.GetInstanceUid(), err)
		return response
	}
	cp.connMu.Lock()
	cp.connections[conn] = append([]byte(nil), message.GetInstanceUid()...)
	cp.connMu.Unlock()

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

func (cp *controlPlane) saveStatus(ctx context.Context, message *protobufs.AgentToServer) error {
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
	_, err = cp.db.Exec(ctx, `
INSERT INTO agents (
    instance_uid, agent_description, health, effective_config, remote_config_status, connected, last_seen
) VALUES ($1, $2::jsonb, $3::jsonb, $4::jsonb, $5::jsonb, TRUE, NOW())
ON CONFLICT (instance_uid) DO UPDATE SET
    agent_description = EXCLUDED.agent_description,
    health = EXCLUDED.health,
    effective_config = EXCLUDED.effective_config,
    remote_config_status = EXCLUDED.remote_config_status,
    connected = TRUE,
    last_seen = NOW()`,
		message.GetInstanceUid(), description, health, effectiveConfig, configStatus)
	return err
}

func marshalOptional(message proto.Message) (string, error) {
	if message == nil || !message.ProtoReflect().IsValid() {
		return `{}`, nil
	}
	encoded, err := protojson.Marshal(message)
	return string(encoded), err
}

func (cp *controlPlane) onDisconnect(conn types.Connection) {
	cp.connMu.Lock()
	instanceUID, connected := cp.connections[conn]
	delete(cp.connections, conn)
	cp.connMu.Unlock()
	if !connected {
		return
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
	mux.HandleFunc("GET /", cp.adminPage)
	mux.HandleFunc("GET /api/v1/agents", cp.listAgents)
	mux.HandleFunc("PUT /api/v1/agents/{uid}/config", cp.putConfig)
	mux.HandleFunc("GET /api/v1/agents/{uid}/config", cp.getConfig)
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

func (cp *controlPlane) adminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>OpAMP Control Plane</title>
<style>body{font:16px system-ui;max-width:960px;margin:2rem auto;padding:0 1rem}textarea{width:100%;height:16rem}table{border-collapse:collapse;width:100%}td,th{padding:.5rem;border:1px solid #ccc;text-align:left}button{padding:.5rem}</style>
<h1>OpAMP Control Plane (staging)</h1>
<p>Agent authentication and TLS are not enabled. Use only on a trusted local staging host.</p>
<button onclick="loadAgents()">Refresh agents</button><pre id="result"></pre>
<label>Agent instance UID (hex)<input id="uid" style="display:block;width:100%"></label>
<label>Desired Collector YAML<textarea id="config"></textarea></label>
<button onclick="saveConfig()">Save configuration</button>
<script>
async function loadAgents(){const r=await fetch('/api/v1/agents');document.querySelector('#result').textContent=JSON.stringify(await r.json(),null,2)}
async function saveConfig(){const uid=document.querySelector('#uid').value.trim();const config=document.querySelector('#config').value;const r=await fetch('/api/v1/agents/'+encodeURIComponent(uid)+'/config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({config})});const body=await r.text();if(!r.ok)throw Error(body);alert('Configuration saved. It will be offered on the agent\\'s next status report.')}
loadAgents().catch(e=>document.querySelector('#result').textContent=e.message)
</script></html>`))
}

func (cp *controlPlane) listAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := cp.db.Query(r.Context(), `
SELECT encode(instance_uid, 'hex'), agent_description, health, effective_config, remote_config_status, connected, last_seen
FROM agents ORDER BY last_seen DESC`)
	if err != nil {
		http.Error(w, "could not list agents", http.StatusInternalServerError)
		cp.logger.Printf("list agents: %v", err)
		return
	}
	defer rows.Close()

	type agent struct {
		InstanceUID        string          `json:"instance_uid"`
		AgentDescription   json.RawMessage `json:"agent_description"`
		Health             json.RawMessage `json:"health"`
		EffectiveConfig    json.RawMessage `json:"effective_config"`
		RemoteConfigStatus json.RawMessage `json:"remote_config_status"`
		Connected          bool            `json:"connected"`
		LastSeen           time.Time       `json:"last_seen"`
	}
	agents := make([]agent, 0)
	for rows.Next() {
		var item agent
		if err := rows.Scan(&item.InstanceUID, &item.AgentDescription, &item.Health, &item.EffectiveConfig, &item.RemoteConfigStatus, &item.Connected, &item.LastSeen); err != nil {
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
