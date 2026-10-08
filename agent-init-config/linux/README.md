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

From this directory on the VM, use the server address that the VM can reach:

```sh
sudo bash ./install-agent.sh "ws://10.0.0.10:4320/v1/opamp"
```

Replace `10.0.0.10` with the reachable IP address or DNS name of the control
plane. Before installing, create a **Bootstrap token** in the control-plane
UI. The installer will securely prompt for the token without echoing it; do not
append the token to the command line. The initial install writes the token as
an `Authorization: Bearer ...` header in `/etc/opamp/supervisor.yaml`.

The token is bound to the first instance UID that connects. Keep the token
private: although it is stored as a hash on the server, the current OpAMP
transport is plaintext `ws://` and the credential can be intercepted on the
network. Use only on a trusted, isolated network, not the public internet.

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
