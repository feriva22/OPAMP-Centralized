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

4. Open <http://localhost:4321> and sign in with `OPAMP_ADMIN_USERNAME` and
   `OPAMP_ADMIN_PASSWORD`. The admin panel uses a signed, `HttpOnly`,
   `SameSite=Strict` session cookie that expires after 12 hours. This serves
   the Preact-based Admin panel embedded in the control-plane binary; no
   separate frontend service is needed.

The OpAMP endpoint is `ws://localhost:4320/v1/opamp`. Both ports are published
according to the bindings in `docker-compose.yml`; PostgreSQL has no published
host port, and its data is stored in the `postgres_data` Docker volume. Do not
expose either HTTP service to an untrusted network.

Stop the services with `docker compose down`. This retains the database volume.
To delete the stored staging data as well, run `docker compose down --volumes`.

## GitHub Actions image publishing

The workflow at `.github/workflows/publish-image.yml` builds the multi-stage
image and publishes it to GitHub Container Registry as
`ghcr.io/<owner>/<repository>`. It runs on branch pushes, `v*` tags, and
pull requests. Pull requests build the image for verification without
publishing it. The default branch also receives the `latest` tag; branch and
version tags are published by their corresponding pushes. Images also receive
a commit-SHA tag.

No registry password setup is required: the workflow uses the built-in
`GITHUB_TOKEN`. Ensure repository Actions are enabled and the workflow has
permission to write packages. The package can be made public from its GitHub
Packages settings if it should be pullable without authentication.

To run a published image with Compose, set `OPAMP_IMAGE` to the desired GHCR
tag (for example `ghcr.io/<owner>/<repository>:latest`) in the environment or
`.env`, then run:

```powershell
docker compose pull opamp-server
docker compose up -d --no-build opamp-server
```

The default Compose behavior remains a local source build tagged
`opamp-control-plane:local`.

## Kubernetes / Argo CD

Kustomize manifests for the control plane and PostgreSQL, plus an Argo CD
Application template, are in [deploy/k8s](deploy/k8s/README.md). Configure the
GHCR image name and provide the required Kubernetes Secret outside Git before
syncing the Application.

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
the token without echoing it and writes it to
`/etc/opamp/supervisor.yaml` as `server.headers.Authorization`. The file is
owned by `root:opamp` with mode `0640`. The control plane stores only a hash of
the token in PostgreSQL; the plaintext is shown only once in the UI and is not
recoverable from the server.

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
list to save a configuration. The API is protected by the same signed session
as the UI. For command-line API use, first log in and save the session cookie:

```sh
curl -c opamp-cookies.txt \
  -H "Content-Type: application/json" \
  -X POST http://localhost:4321/api/v1/login \
  --data "{\"username\":\"$OPAMP_ADMIN_USERNAME\",\"password\":\"$OPAMP_ADMIN_PASSWORD\"}"
```

```text
PUT /api/v1/agents/{instance_uid}/config
Content-Type: application/json

{"config":"receivers:\n  otlp:\n    protocols:\n      grpc:\n"}
```

For example, from a shell that has loaded the operator credentials:

```sh
curl -b opamp-cookies.txt \
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
- Replace static environment-based operator credentials with the
  organization's identity system, add roles and audit controls, and protect
  the UI/API with TLS. The static single-user login is only suitable for
  isolated staging.
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
