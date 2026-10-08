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
   Auth credentials.

The OpAMP endpoint is `ws://localhost:4320/v1/opamp`. Both ports are published
only on loopback. PostgreSQL has no published host port, and its data is stored
in the `postgres_data` Docker volume.

Stop the services with `docker compose down`. This retains the database volume.
To delete the stored staging data as well, run `docker compose down --volumes`.

## Features

- Persists agent UID, description, health, effective config, remote-config
  status, and last-seen/connected state in PostgreSQL.
- Lists agents in the Basic-Auth-protected UI and JSON API, including saved
  desired configs and human-readable effective config files reported by agents.
- Saves versioned desired Collector configurations and offers them to agents
  in OpAMP responses.
- Provides `GET /healthz` for an unauthenticated database readiness check.
- Runs the app as a non-root user with a read-only filesystem, drops Linux
  capabilities, and keeps the database on a private Docker network.

## Linux VM agents

Bootstrap the OpenTelemetry Collector Contrib and OpAMP Supervisor on Debian
or Ubuntu VMs with the installer in
[agent-init-config/linux](agent-init-config/linux/README.md). It uses the
current plaintext staging endpoint by default; the VM must be able to reach
the server, and agent authentication/TLS are still production blockers.

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

## Production blockers

**Do not expose this staging setup to an untrusted network.** Before production,
the deployment and application must be extended and security-reviewed:

- Enable TLS (`wss://`) with certificates valid for the deployment hostname.
  The current service accepts plaintext WebSockets only.
- Authenticate and authorize every agent with unique credentials (or mTLS),
  map each credential to an allowed agent identity, and support rotation and
  revocation. Currently the OpAMP listener accepts connections without agent
  authentication.
- Replace Basic Auth with the organization's operator identity system, add
  roles and audit controls, and protect the UI/API with TLS. Basic Auth here is
  only suitable for loopback staging.
- Validate and test configs before rollout; add staged rollout, approval,
  rollback, and apply-result workflows.
- Configure encrypted backups, secret management, monitoring, resource limits,
  upgrades, and disaster recovery. The local Compose file is single-host and
  does not provide high availability.

Changing the Compose port bindings or putting this service behind a TLS proxy
does **not** by itself add agent authentication or make the app production
ready.
