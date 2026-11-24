# Build from source

Self-built bundles work like official releases, except in-app update checks are disabled.

## Build a bundle

Requires Git, Go 1.27+, Node 22.22+ with npm, `curl`, and `unzip` on macOS or Linux. The build downloads the pinned Xray release listed in `deploy/xray.pin`, verifies its SHA-256 checksum, and prints the resulting bundle's checksum and path.

```sh
git clone https://github.com/MeafQ/sidegate && cd sidegate
bundle=$(scripts/build-bundle.sh)
scp "$bundle" root@GATEWAY:sidegate.tar.gz
```

## Manual installation

Download `sidegate-vX.Y.Z-arm64.tar.gz` and `SHA256SUMS` from a release (or copy your local bundle as above), then on the gateway as `root`, verify the checksum and extract `sidegated`:

```sh
sha256sum sidegate.tar.gz
tar -xzf sidegate.tar.gz sidegated
```

Preview the plan, then install:

```sh
./sidegated install --bundle sidegate.tar.gz
./sidegated install --bundle sidegate.tar.gz --yes
```

- To upgrade an existing installation (or switch from a local build to an official release), use `upgrade` instead of `install`.
- To supply the UniFi API key during CLI install instead of in the web UI, pass `--api-key-stdin` (paste the key, press Enter, then Ctrl-D).
