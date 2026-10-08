# Linux VM OpAMP agent bootstrap

This directory installs the OpenTelemetry Collector Contrib and the OpAMP
Supervisor as a systemd service on Debian/Ubuntu Linux VMs. The Supervisor
manages the Collector and reports its state to the OpAMP server.

## Requirements

- Debian or Ubuntu VM with systemd.
- `curl`, `tar`, and `sha256sum`.
- `root`/`sudo` access and outbound HTTPS access to GitHub releases.
- Network access from the VM to the OpAMP server.

The script supports Linux `amd64` and `arm64`. It pins both binaries to
`0.159.0` by default and verifies each download against the SHA-256 checksum
published with its GitHub release. Set `SUPERVISOR_VERSION` and
`COLLECTOR_VERSION` to change the versions.

## Install

1. Create a **Bootstrap token** in the control-plane UI. Keep the page open;
   the plaintext token is shown only once.
2. Connect to the Debian or Ubuntu VM that will run the agent. Ensure it can
   reach the OpAMP server and has `git`, `curl`, and `sudo`.
3. Clone the repository and move to the Linux installer directory:

```sh
git clone https://github.com/feriva22/OPAMP-Centralized.git
cd OPAMP-Centralized/agent-init-config/linux
```

4. Run the installer, replacing `YOUR_OPAMP_SERVER` with the DNS name or IP
   address reachable from the VM:

```sh
sudo bash ./install-agent.sh "ws://YOUR_OPAMP_SERVER:4320/v1/opamp"
```

Use the OpAMP WebSocket endpoint, including `/v1/opamp`, not the control-panel
HTTP endpoint. If the OpAMP listener is published on another external host or
port, use that reachable address instead. The installer securely prompts for
the token without echoing it; paste the Bootstrap token from the UI and press
Enter. Do not append the token to the command line. The initial install stores
the token in `/etc/opamp/supervisor.yaml` as the Supervisor's
`server.headers.Authorization` value. The file is owned by `root:opamp` with
mode `0640` (root read/write and `opamp` group read).

The resulting Supervisor config has this structure. The token below is a
placeholder, not a real credential:

```yaml
server:
  endpoint: "ws://YOUR_OPAMP_SERVER:4320/v1/opamp"
  headers:
    Authorization: "<TOKEN_SHOWN_ONCE>"
```

The control plane stores the SHA-256 digest in PostgreSQL as a 32-byte binary
value in `agent_tokens.token_hash`. It does not store this plaintext config
value and cannot reveal it again.

The token is bound to the first instance UID that connects. Keep the token
private: the control plane stores only a token hash in PostgreSQL, but the
agent needs the plaintext token in its protected Supervisor config to
authenticate. The current OpAMP transport is plaintext `ws://`, so the
credential can also be intercepted on the network. Use only on a trusted,
isolated network, not the public internet.

The installer downloads the binaries, creates the `opamp` system account,
installs the Collector at `/opt/opamp/bin/otelcol-contrib`, copies the config
files to `/etc/opamp/` (without replacing existing configs), and enables
`opamp-supervisor.service`. It detects whether either binary is already
installed: if its reported version matches the requested version, it reuses
that binary rather than downloading it again; otherwise it downloads and
verifies the requested release. Existing Collector and Supervisor config files
are preserved on reinstall.

Check it with:

```sh
sudo systemctl status opamp-supervisor
sudo journalctl -u opamp-supervisor -f
```

The starting Collector config scrapes basic host metrics and writes them to
the `debug` exporter (the service journal). Replace its exporter/pipeline with
your telemetry backend settings before rolling out to real VMs. The control
plane can then offer per-agent remote Collector configuration.

## Credential rotation and revocation

Use **Rotate** or **Revoke** in the control-plane UI. Rotation revokes the old
token immediately and disconnects the VM, then displays the replacement only
once. Update the `Authorization` header in `/etc/opamp/supervisor.yaml`:

```yaml
server:
  endpoint: "ws://10.0.0.10:4320/v1/opamp"
  headers:
    Authorization: "Bearer REPLACE_WITH_NEW_TOKEN"
```

Then restart and check the service:

```sh
sudo systemctl restart opamp-supervisor
sudo journalctl -u opamp-supervisor -f
```

The installer preserves an existing Supervisor config, so it does not
overwrite tokens or local settings during a reinstall. The config is installed
root-owned with mode `0640`, readable by the `opamp` service account.

## Security warning

Agent authentication is enabled, but the staging OpAMP endpoint still uses
plaintext `ws://`; bearer tokens and telemetry are not encrypted in transit.
Before any production use, enable and verify `wss://` on the OpAMP listener and
protect the operator UI/API with TLS and stronger operator authentication. Do
not put real tokens in this repository.

For server-side configuration and remaining production gaps, see the main
[project README](../../README.md).
