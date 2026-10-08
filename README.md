# OpAMP control plane (local/staging)

This repository provides a small OpAMP server with PostgreSQL-backed agent
status and desired-configuration storage. It uses the OpenTelemetry
[OpAMP Go server implementation](https://github.com/open-telemetry/opamp-go).
It is a local/staging foundation, **not yet safe to expose to production
networks**.

## Start on Windows

1. Copy `.env.example` to `.env`.
2. Set `OPAMP_ADMIN_USERNAME`, `OPAMP_ADMIN_PASSWORD` (at least 16 characters),
   and `POSTGRES_PASSWORD` in `.env`. Use unique, randomly generated
   alphanumeric values (the database password is embedded in a connection
   URL); keep `.env` private and out of source control.
3. Start the services:

   ```powershell
   docker compose up --build -d
   ```

4. Open <http://localhost:4321>. The browser prompts for the operator Basic
   Auth credentials. This serves the Preact-based Admin panel embedded in the
   control-plane binary; no separate frontend service is needed.

The OpAMP endpoint is `ws://localhost:4320/v1/opamp`. Both ports are published
according to the bindings in `docker-compose.yml`; PostgreSQL has no published
host port, and its data is stored in the `postgres_data` Docker volume. Do not
expose either HTTP service to an untrusted network.

Stop the services with `docker compose down`. This retains the database volume.
To delete the stored staging data as well, run `docker compose down --volumes`.

## Features

- Persists agent UID, description, health, effective config, remote-config
  status, and last-seen/connected state in PostgreSQL.
- Lists agents in the Basic-Auth-protected UI and JSON API, including saved
  desired configs and human-readable effective config files reported by agents.
- Provides a responsive Admin panel with fleet overview, agent details and
  configuration editor, plus dedicated credential management.
- Shows agent hostname, OS type and description (for example, Ubuntu 26.04),
  service/type name, version, and source IP when the agent reports those
  resource attributes and connects directly to the server.
- Issues random per-agent bearer tokens, binds bootstrap tokens to the first
  connecting instance UID, stores only token hashes, and supports rotation and
  revocation from the operator UI. Revoking a credential rejects new
  connections and disconnects its active connection.
- Saves versioned desired Collector configurations and offers them to agents
  in OpAMP responses.
- Provides `GET /healthz` for an unauthenticated database readiness check.
- Runs the app as a non-root user with a read-only filesystem, drops Linux
  capabilities, and keeps the database on a private Docker network.

## Linux VM agents

Bootstrap the OpenTelemetry Collector Contrib and OpAMP Supervisor on Debian
or Ubuntu VMs with the installer in
[agent-init-config/linux](agent-init-config/linux/README.md). It uses the
plaintext staging endpoint; the VM must be able to reach the server. Create a
bootstrap credential in the UI before installing. The installer prompts for
the token without echoing it and writes it to the root-owned Supervisor config,
readable only by the `opamp` service account.

Tokens are displayed only once. A bootstrap token is initially unbound and is
locked to the first instance UID that uses it; subsequent use by a different
UID is rejected. To enroll a known agent, use **Issue token** in its agent row.
The credential table lets an operator revoke a token or rotate it. Rotation
immediately revokes and disconnects the old credential, so update the VM's
`Authorization` header to the newly displayed token and restart
`opamp-supervisor` promptly. Existing VM config files are not overwritten by
the installer.

Deploying this version immediately rejects existing Supervisor connections
that do not send a token. For an upgrade, plan a brief reconnect window: issue
a token for each known agent in the UI, add its `Authorization` header to that
VM's Supervisor config, and restart the service.

The API provides the same Basic-Auth-protected operations:

```text
POST   /api/v1/agent-tokens                   {"name":"Linux VM bootstrap"}
POST   /api/v1/agents/{instance_uid}/token    {"name":"Production VM"}
GET    /api/v1/agent-tokens
POST   /api/v1/agent-tokens/{id}/rotate
DELETE /api/v1/agent-tokens/{id}
```

The create and rotate responses contain the plaintext `token` once; list
responses never contain it. Treat creation/rotation responses as secrets and
do not save them in logs or shared shell history.

Use an agent's 32-character hexadecimal (16-byte) instance UID from the agent
list to save a configuration. The API is protected by the same Basic Auth as
the UI:

```text
PUT /api/v1/agents/{instance_uid}/config
Content-Type: application/json

{"config":"receivers:\n  otlp:\n    protocols:\n      grpc:\n"}
```

For example, from a shell that has loaded the operator credentials:

```sh
curl -u "$OPAMP_ADMIN_USERNAME:$OPAMP_ADMIN_PASSWORD" \
  -H "Content-Type: application/json" \
  -X PUT http://localhost:4321/api/v1/agents/0123456789abcdef0123456789abcdef/config \
  --data '{"config":"receivers:\n  otlp:\n    protocols:\n      grpc:\n"}'
```

Configurations are recorded in the database as revisions. The server sends
the desired config when the agent next reports status. This staging
implementation does not validate Collector configuration before offering it.

Agent metadata is read from OpAMP identifying attributes (`host.name`,
`os.type`, `service.name`, and `service.version`); a field stays blank if the
agent does not report it. Source IP is read from the TCP connection peer. If
agents connect through a proxy, this will be the proxy address; forwarded
headers are intentionally not trusted.

## Production blockers

**Do not expose this staging setup to an untrusted network.** Per-agent
authentication is now enabled, but before production the deployment and
application must still be extended and security-reviewed:

- Enable TLS (`wss://`) with certificates valid for the deployment hostname.
  The current OpAMP service uses plaintext WebSockets, so bearer tokens can be
  intercepted and replayed by anyone able to observe the connection.
- Replace Basic Auth with the organization's operator identity system, add
  roles and audit controls, and protect the UI/API with TLS. Basic Auth here is
  only suitable for isolated staging.
- Consider short-lived credentials or mTLS, credential expiry, and audited
  credential-management events before production use.
- Validate and test configs before rollout; add staged rollout, approval,
  rollback, and apply-result workflows.
- Configure encrypted backups, secret management, monitoring, resource limits,
  upgrades, and disaster recovery. The local Compose file is single-host and
  does not provide high availability.

Putting the service behind a TLS proxy does not enable TLS on its OpAMP
listener; use a secure `wss://` endpoint for production agents.

## Admin panel frontend development

Frontend source is in `cmd/controlplane/ui`. To build its static bundle locally:

```powershell
cd cmd\controlplane\ui
npm ci
npm run build
```

The Go server embeds `cmd/controlplane/ui/dist`, and the Docker build compiles
the frontend before building the server image.
