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
plane. The current Compose deployment binds OpAMP to `127.0.0.1`, so a separate
VM cannot connect to it unless the server is deliberately made reachable on a
private network. Do not publish it to the public internet.

The installer downloads the binaries, creates the `opamp` system account,
installs the Collector at `/opt/opamp/bin/otelcol-contrib`, copies the config
files to `/etc/opamp/` (without replacing existing configs), and enables
`opamp-supervisor.service`.

Check it with:

```sh
sudo systemctl status opamp-supervisor
sudo journalctl -u opamp-supervisor -f
```

The starting Collector config scrapes basic host metrics and writes them to
the `debug` exporter (the service journal). Replace its exporter/pipeline with
your telemetry backend settings before rolling out to real VMs. The control
plane can then offer per-agent remote Collector configuration.

## Security warning

The current control-plane OpAMP endpoint uses plaintext `ws://` and accepts
agents without authentication. The script intentionally configures that
staging behavior; it is not suitable for an untrusted network or sensitive
telemetry. Before deploying beyond a trusted, isolated test network, add TLS
(`wss://`) and per-agent authentication to the server, issue/rotate individual
agent credentials, and update the Supervisor config to use TLS and those
credentials. Do not put shared secrets in this repository.

For server-side configuration and remaining production gaps, see the main
[project README](../../README.md).
