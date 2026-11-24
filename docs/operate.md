# Operate sidegate

Run all CLI commands over SSH on the gateway as `root`.

## Update

When a newer version is available, an **Update** button appears in **Settings → About**. Click it and confirm; the page reloads automatically once the new version is running (if you have unsaved edits, a **Reload** prompt appears instead).

Opening the web UI checks for updates at most once every 24 hours unless you uncheck **Check for updates** in **Settings → About**. Details on what the check sends are in [web/API.md](../web/API.md#updates).

You can run the same update over SSH (dry-run plan first):

```sh
/data/sidegate/current/sidegated update
/data/sidegate/current/sidegated update --yes
```

Inspect update logs with `journalctl -u sidegate-update`. To downgrade to an older version, [install its bundle manually](build.md#manual-installation).

## Recovering from a failed update

If the new release fails to start or cannot keep Xray running, the updater automatically rolls back and **Settings → About** reports **Rolled back**.

If an update is interrupted mid-switch (for example, by a power loss), **About** may show **Interrupted** or the page may stop responding. Recovery runs automatically the next time `sidegated` starts. To trigger it immediately:

```sh
/data/sidegate/rollback/sidegated recover --yes && systemctl restart sidegated.service
```

Recovery succeeded once the UI responds and shows either the previous version (**Rolled back**) or the new version (if the switch had already completed). **Stopped** means nothing had been switched yet. If `/data/sidegate/rollback/sidegated` does not exist, no rollback is pending. If another lifecycle command holds the lock, wait a moment and retry.

If the page still doesn't respond or reports **Recovery needed**, [install a known-good bundle manually](build.md#manual-installation). If `upgrade` reports that the rollback record is unreadable due to disk corruption, move it aside (giving up automatic rollback) and re-run `upgrade`:

```sh
mv /data/sidegate/rollback /data/sidegate/rollback.damaged
```

## After a UniFi firmware update

If a UniFi OS firmware update clears `/etc/systemd/system`, sidegate won't start at boot and all slots stay closed. Re-register the units from the persisted `/data` installation:

```sh
/data/sidegate/current/sidegated install --yes
```

## Remove the nginx hook

If the UniFi web UI breaks and you need to take `/sidegate/` out of nginx immediately, stop `sidegated` first (otherwise it re-installs the hook within a minute). Active proxy connections stay up via Xray without automatic failover.

```sh
systemctl disable --now sidegated.service
rm /data/unifi-core/config/http/shared-runnable-sidegate.conf
nginx -t && nginx -s reload
```

Run `/data/sidegate/current/sidegated install --yes` to restore the hook and service.

## Uninstall

First move or remove any UniFi Policy Table routes pointing to sidegate VPN Clients. Leftover routes will hit UniFi's kill switch or fall back to WAN. Do not delete sidegate VPN Clients manually in UniFi, as UniFi deletes attached traffic routes with them.

Omit `--yes` on either command to preview the plan without making changes:

```sh
/data/sidegate/current/sidegated uninstall --yes
/data/sidegate/current/sidegated uninstall --purge --yes
```

- **`uninstall --yes`** removes services, binaries, nginx config, and analytics, but preserves sources, groups, slots, your API key, and UniFi VPN Clients so a future reinstall picks up cleanly.
- **`uninstall --purge --yes`** also deletes `/data/sidegate` and removes sidegate-owned VPN Clients that have no Policy Table routes pointing to them (any routed or undeletable clients are left untouched and listed in the output).
