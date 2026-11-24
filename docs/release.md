# Release process

## Before the first tag

1. Set the final repository URL and replace `OWNER` in [README.md](../README.md#install) and [build.md](build.md#build-a-bundle). Note that bundles pin this URL for in-app updates ([details below](#compatibility-contract)).
2. Ensure the default branch is `main`.
3. Confirm [LICENSE](../LICENSE) (MIT) and [deploy/XRAY-LICENSE](../deploy/XRAY-LICENSE) (MPL-2.0) are in place.
4. Ensure no private data, internal notes, or broken links are in the repository or its git history.
5. Confirm every GitHub Action in [release.yml](../.github/workflows/release.yml) is pinned to a full commit SHA.
6. In GitHub repository settings, enable immutable releases, restrict `v*` tag pushes to the repository owner, allow Actions to create releases, and protect the owner account with a hardware security key. Anyone who can publish a release controls root execution on gateways that click **Update**.
7. Run `scripts/quick.sh` and the full fuzz suite cleanly on `main`.

The first real release validates the workflow trigger, release publishing, `latest/download` asset resolution, and the [remaining systemd 257 checks](architecture.md#unverified-on-systemd-257).

## Cutting a release

1. Run the [lab checks](../lab/README.md#checks) against `main`.
2. Push one semver tag at a time (`vX.Y.Z`, strictly greater than the current Latest, no leading zeros):

	```sh
	git tag vX.Y.Z
	git push origin vX.Y.Z
	```

3. The release workflow validates the tag, builds the arm64 bundle, verifies every asset, creates a draft release, and publishes it (marking it Latest if higher than all existing releases).
4. If the workflow fails before publishing, re-run it — the Publish step replaces any leftover draft. Never reuse an already-published tag; push a new patch version instead.

After publishing, verify:

- The release is marked **Latest** and contains the tarball, `sidegate.json`, `SHA256SUMS`, `install.sh`, and `XRAY-LICENSE`.
- The one-line install command installs Latest cleanly on a fresh lab VM.
- Starting with the second release, an older gateway detects the update in **Settings → About** and upgrades cleanly.

## Compatibility contract

Because the currently installed `sidegated` initiates an update and the new bundle's `sidegated` performs the switch (and writes rollback state the old daemon may have to read), every release must preserve:

- **Releases URL.** Keep the original releases URL reachable (GitHub redirects after a repo rename work, but `RELEASES` in `release.yml` must still be built with the original address for in-place updates; otherwise users must upgrade manually).
- **Release assets.** `latest/download/sidegate.json` (`version`, `bundle`, `size`, `sha256`, `core`, ≤ 64 KiB) and `download/vX.Y.Z/sidegate-vX.Y.Z-arm64.tar.gz` containing only `sidegated` and `xray` (≤ 256 MiB), plus `install.sh` and `SHA256SUMS`.
- **CLI contract:**
	- `version` prints the version and releases URL on two lines.
	- `upgrade --data-dir DIR --bundle FILE --yes --lock-fd 3 --expect RELEASE --restarts-core=yes|no --outcome FILE`, writing `{outcome, message}` where `outcome` is one of `landed`, `landedWithError`, `refused`, `rolledBack`, `putBackFailed`, `busy`, `moved`, or `planChanged`.
	- `recover --data-dir DIR --yes`, `settle-update --data-dir DIR`, `serve --data-dir DIR`, `close-gates --run-dir DIR`, and `update --yes`.
- **Systemd units & files.** Unit names `sidegated.service`, `sidegate-core.service`, `sidegate-update`; `ExecStartPre`/`ExecStart` recovery wiring; `rollback/` journal layout (`sidegated`, `record.json`, `started`); `update/attempt.json` and `update/<attempt>/outcome.json`; `/data/sidegate` layout and `/run/sidegate-lifecycle.lock`.
- **HTTP responses.** `release` in `api/session` and backwards-compatible schemas for `api/session` and `api/update` (an older open browser tab rejects unknown fields or enum values).
- **Database schemas.** `state.db` and `metrics.db` currently refuse mismatched schema versions without migrating. Add forward-compatible migrations (or backup/restore on rollback) before any release that bumps either schema version.
