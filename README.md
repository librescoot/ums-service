# Librescoot UMS Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

The UMS Service provides the vehicle's USB gadget workflow. It switches the MDB USB connection between Ethernet and USB mass-storage modes, presents a managed FAT drive to a host, then imports supported files and coordinates resulting update/reboot work through Redis or Valkey.
## Capabilities

- Switches between `g_ether` (normal) and `g_mass_storage` (UMS) USB gadget modules.
- Creates and manages a 1 GiB FAT-backed virtual drive at `/data/usb.drive` by default.
- Exports and imports settings, WireGuard configuration, selected service configuration, and an optional boot script.
- Queues full (`.mender`) and delta (`.delta`) MDB and DBC update artifacts, transfers DBC-bound content through the DBC interface, and waits for queued installation status before a permitted reboot.
- Imports supported map archives and MDB/DBC scripts.
- Collects diagnostics and exposes saved log bundles on the virtual drive.
- Tracks USB attach/detach state, supports the two-detach `ums-by-dbc` flow, and updates the `usb` Redis/Valkey hash with mode, status, and processing step.
- Prunes retained log bundles and stale OTA artifacts at startup and after UMS cycles.

## Operation and interfaces

The service watches the `mode` field of the `usb` hash. Set the field and publish the `usb` channel using the normal Redis/Valkey hash-notification convention:

```sh
redis-cli HSET usb mode ums
redis-cli PUBLISH usb mode
```

Supported modes are:

| Mode | Behaviour |
| --- | --- |
| `normal` | Uses the Ethernet gadget and processes any completed UMS cycle. |
| `ums` | Presents the virtual mass-storage drive; the first detected USB-host detach returns to normal mode. |
| `ums-by-dbc` | Presents the same drive but waits for a second detected detach before returning to normal mode. |

At startup the service seeds `usb.mode=normal` and `usb.status=idle`. During a cycle it publishes statuses including `preparing`, `active`, `processing`, `awaiting-reboot`, `rebooting`, and `idle`; `usb.step` identifies the current import stage. While `awaiting-reboot`, the step names what the wait is on: `waiting-mdb`, `waiting-dbc`, or `waiting-dbc+mdb`. MDB-bearing updates then use `waiting-vehicle-state`; every DBC update remains at `waiting-dbc` until update-service reports the final post-reboot outcome, including combined DBC+MDB imports. The install wait is not a fixed deadline: while an install is genuinely progressing (downloading, preparing, installing, or a DBC verifying its post-reboot commit), the wait keeps being extended up to a 2-hour overall cap, so slow delta installs are not abandoned before the reboot is triggered. Only a wait that ends with no install activity — or the overall cap — records a `timeout` result and retains MDB reboot ownership as a fail-safe. A staged push that resolves to nothing applicable is the exception: update-service publishes `staged-noop`, the cycle finishes at once, and no reboot is triggered.

When the reboot phase finishes, `usb.status` returns to `idle` and the outcome is recorded on `usb.last-result` (`reboot-triggered`, `timeout`, `install-error`, `vehicle-state`, `nothing-to-install`, or `error`), with `usb.last-result-detail` and `usb.last-result-time` alongside it. The next UMS entry clears all three. `lsc usb status` prints them, and `lsc usb log` shows the per-cycle entries from the `usb:log` list. Per-cycle detail is also written to the virtual drive as `ums_log.txt`.

The exported drive contains managed areas for `settings.toml`, `wireguard/`, `radio-gaga/`, `uplink-service/`, `onboot.sh`, `system-update/`, `maps/`, `scripts/`, `log-bundles/`, and `diagnostics/`. On return to normal mode, the service copies supported configuration back to its managed locations, processes imports, restarts affected services when configuration changed, cleans the drive, and unmounts it.

Update artifacts placed in `system-update/` are grouped per board (`librescoot-*-mdb-*`, `librescoot-*-dbc-*`) and staged in the canonical location update-service already uses: `/data/ota/mdb` on the MDB, or `/data/ota/dbc` on the DBC. UMS then pushes one path-free command per board, `apply-staged-updates`, to that board's update-service command list; update-service discovers the staged files and resolves what to install.

| Files for one board | Result |
| --- | --- |
| one `.mender` or one `.delta` | staged and installed |
| two or more `.delta` on one channel | staged; update-service resolves the chain and installs it as one image |
| more than one `.mender`, or a `.mender` together with any `.delta` | refused |
| deltas spanning more than one channel | refused |

A refused board is skipped without staging anything, while any other board in the same drop is processed normally. A refusal is reported in three places: the journald log, `usb:log`, and the dashboard: `usb.last-result` is set to `error` (so `lsc usb status` shows it) and an `error` notification is published on `scootui:notification`. When the other board in the same drop proceeds and installs, the refusal is folded into the terminal outcome's `usb.last-result-detail`, so `lsc usb status` still shows it after the install finishes.

On entry from normal mode the service discards any update artifact still present in `system-update/`; a re-entry while the drive is still exported (for example `ums` to `ums-by-dbc`) leaves host-written files in place. A completed cycle wipes the drive on its way out, so a file found there at entry is the residue of a cycle that died before it exited; it is removed and the discard is recorded in `usb:log`. The `/data/ota` staging directories and update-service's delta base are never touched.

update-service owns the final discovery: the running version's base `.mender` sits permanently in that directory and is ignored, a newer `.mender` is installed as a full image, and a delta chain is resolved from the deltas' own metadata and applied as one mender install with one reboot.

Do not remove the virtual drive file or force-unload its gadget module while a host is writing it. Let the host detach and allow the service to complete its processing cycle.

## Configuration

The service has no general command-line configuration; `--version` or `-version` prints the build version. Configure it with environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `REDIS_ADDR` | `localhost:6379` | Redis/Valkey address |
| `REDIS_PASSWORD` | empty | Read into configuration but not currently passed to the Redis/Valkey IPC client |
| `UMS_MAP_TIMEOUT` | `10m` | Per-map DBC-transfer timeout |
| `UMS_SCRIPT_TIMEOUT` | `2m` | Per-script DBC-transfer timeout |
| `UMS_MENDER_TIMEOUT` | `15m` | Per-Mender DBC-transfer timeout |

Invalid timeout values are logged and fall back to their defaults. The virtual-drive path and size are currently configured in the program as `/data/usb.drive` and 1 GiB.

## Build and test

A Go toolchain is required. The default target cross-compiles a Linux ARMv7 binary.

```sh
make build        # bin/ums-service for ARMv7
make build-amd64  # bin/ums-service-amd64
make test
```

`make lint` and `make clean` are also available.

## Deployment and runtime dependencies

The image recipe installs `/usr/bin/ums-service` and `librescoot-ums.service`. The unit runs as `root`, requires `valkey.service`, starts after the vehicle service, and restarts automatically.

Runtime operation requires:

- Redis or Valkey and the services that consume queued update/hardware/power commands;
- kernel USB gadget support, `g_ether`, and `g_mass_storage`;
- `modprobe` and `rmmod` for gadget switching;
- filesystem and mount tooling sufficient to create, format, mount, and unmount the virtual drive;
- writable `/data` storage; and
- the configured DBC interface for DBC updates, maps, or scripts; it uses SSH for reachability and remote commands, prefers HTTP PUT for file transfer, and falls back to SCP.

```sh
systemctl status librescoot-ums.service
journalctl -u librescoot-ums.service
```

## Operational and security notes

- The systemd unit is intentionally privileged because it loads kernel modules, mounts storage, manages service configuration, and can initiate update/reboot workflows. Do not expose `usb.mode` writes to untrusted Redis/Valkey clients.
- Treat files placed on the UMS drive as privileged inputs. In particular, updates, scripts, VPN configuration, and `onboot.sh` can alter a vehicle after the drive is disconnected.
- The service validates `onboot.sh` syntax before installing it, but that does not make its contents safe. Only supply scripts from trusted operators.
- UMS-triggered MDB reboot is limited to the vehicle states `stand-by`, `parked`, and `shutting-down`; DBC-only reboot is owned by update-service. Check `usb.step` for what a cycle in `awaiting-reboot` is waiting on, and `usb.last-result` for why one gave up.
- Stop the service with `SIGTERM` or `SIGINT`; its USB monitor is stopped as part of context shutdown.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
