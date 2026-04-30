# Librescoot UMS Service

A Go service that manages USB gadget mode switching between network (g_ether) and USB Mass Storage (UMS) modes on embedded Linux devices. The service monitors Redis for mode change commands and handles file transfers between the host computer and the device.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Features

- **USB Mode Switching**: Dynamically switch between network mode (g_ether) and USB mass storage mode
- **Redis Integration**: Monitor Redis for mode change commands via PUBLISH/SUBSCRIBE
- **Virtual USB Drive**: Automatically creates and manages a 1GB FAT32-formatted virtual drive
- **Settings Management**: Sync settings.toml between device and USB drive
- **WireGuard VPN**: Manage WireGuard configuration files (create, update, delete)
- **System Updates**: Process .mender update files for both main board (MDB) and dashboard computer (DBC)
- **Map Updates**: Transfer map files (.mbtiles and tiles.tar) to the dashboard computer
- **Dashboard Computer Interface**: Manage DBC connectivity and file transfers via SSH/HTTP

## Architecture

```
ums-service/
├── cmd/ums-service/      # Main entry point
├── internal/service/     # Service orchestration
└── pkg/
    ├── config/          # Configuration management
    ├── dbc/            # Dashboard Computer interface
    ├── disk/           # Virtual disk operations
    ├── maps/           # Map file updates
    ├── redis/          # Redis pub/sub handling
    ├── settings/       # Settings file management
    ├── update/         # System update handling
    ├── usb/            # USB gadget mode control
    └── wireguard/      # WireGuard config management
```

## Requirements

- Linux with USB gadget support
- Redis server
- Root/sudo access for kernel module operations
- Tools: `modprobe`, `mkfs.fat`, `mount`, `ssh`, `scp`

## Configuration

The service can be configured via environment variables:

- `REDIS_ADDR`: Redis server address (default: `localhost:6379`)
- `REDIS_PASSWORD`: Redis password (default: empty)

## Redis Commands

The service monitors Redis for mode changes:

```bash
# Switch to USB Mass Storage mode (regular)
redis-cli HSET usb mode ums
redis-cli PUBLISH usb mode

# Switch to USB Mass Storage mode (DBC-specific)
# Stays in UMS mode after first disconnect, switches to normal after second disconnect
redis-cli HSET usb mode ums-by-dbc
redis-cli PUBLISH usb mode

# Switch to normal (network) mode
redis-cli HSET usb mode normal
redis-cli PUBLISH usb mode
```

### Mode Behavior

- **ums**: Switches to normal mode after the first USB disconnect
- **ums-by-dbc**: Stays in UMS mode after the first disconnect, only switches to normal after the second disconnect (useful for DBC updates where multiple disconnects may occur)

## USB Drive Structure

When in UMS mode, the virtual drive contains:

```
/
├── settings.toml        # Device settings (bidirectional)
├── onboot.sh            # User boot script (bidirectional, validated on copy-back)
├── wireguard/           # WireGuard VPN configs (bidirectional)
│   └── *.conf
├── radio-gaga/
│   └── config.yaml      # Telemetry uplink config (bidirectional)
├── uplink-service/
│   └── config.yaml      # Uplink service config (bidirectional)
├── system-update/       # Place .mender files here (write-in only)
│   ├── librescoot-mdb-*.mender
│   └── librescoot-dbc-*.mender
├── maps/                # Place map files here (write-in only)
│   ├── *.mbtiles
│   └── *tiles.tar or valhalla_tiles_*.tar
├── log-bundles/         # Saved diagnostic bundles from `lsc logs` (read-only)
│   └── logs-*.tar.gz
└── diagnostics/         # Live system info captured each cycle (read-only)
    ├── mdb/
    └── dbc/
```

## Startup & post-cycle cleanup

On boot and again after every UMS cycle, ums-service performs housekeeping:

- **Log bundles**: keep only the 10 most recent `/data/log-bundles/logs-*.tar.gz`.
- **OTA artifacts**:
  - Remove any `*.mender` / `*.delta` outside the managed dirs (`mdb`, `dbc`, `mdb-boot`, `dbc-boot`).
  - In `mdb/` and `dbc/`, keep only the newest version per channel group (semver-aware for v-prefixed stable versions, lexicographic for ISO-timestamped nightly/testing).
  - In `mdb-boot/` and `dbc-boot/`, keep the 5 newest per group.

Post-cycle cleanup skips pruning of `/data/ota/{mdb,dbc}` because update-service installs queued .mender files asynchronously after our LPush; the next boot's full cleanup sweeps them.

## File Processing

### When switching to UMS mode:
1. Copies `/data/settings.toml` to USB drive (if exists)
2. Copies `/data/wireguard/*.conf` to USB `wireguard/` directory
3. Copies `/data/radio-gaga/config.yaml` to USB `radio-gaga/` directory
4. Copies `/data/uplink-service/config.yaml` to USB `uplink-service/` directory
5. Copies `/data/onboot.sh` to USB drive (if exists)
6. Copies `/data/log-bundles/logs-*.tar.gz` to USB `log-bundles/` directory
7. Creates `system-update` and `maps` directories
8. Captures live diagnostics into USB `diagnostics/` directory

### When switching to normal mode:
1. **Settings**: Copies settings.toml back; restarts settings-service if changed
2. **WireGuard**:
   - Syncs *.conf files from USB to `/data/wireguard/`
   - Removes local configs not present on USB
   - Restarts settings-service if changed
3. **radio-gaga**: Copies USB `radio-gaga/config.yaml` back; restarts `radio-gaga.service` if changed
4. **uplink-service**: Copies USB `uplink-service/config.yaml` back; restarts `librescoot-uplink.service` if changed
5. **onboot.sh**: Validates shebang and shell syntax (`<interp> -n`, falling back to `/bin/sh -n`); installs and chmods +x if valid, otherwise leaves the existing script untouched
6. **Updates**:
   - MDB updates: Installs locally and marks for reboot
   - DBC updates: Transfers to DBC and installs remotely
7. **Maps**: Transfers map files to DBC
8. Runs post-cycle cleanup (see above)
9. Cleans the USB drive
10. Reboots if required by updates

## Building

```bash
# Build for ARM7 (default)
make build

# Build for AMD64
make build-amd64

# Clean build artifacts
make clean

# Run linter
make lint

# Run tests
make test
```

## Running

The service requires root privileges to manage USB gadget modules:

```bash
sudo ./bin/ums-service
```

## File Locations

- Virtual USB drive: `/data/usb.drive`
- Settings: `/data/settings.toml`
- Boot script: `/data/onboot.sh`
- WireGuard configs: `/data/wireguard/`
- radio-gaga config: `/data/radio-gaga/config.yaml`
- uplink-service config: `/data/uplink-service/config.yaml`
- Updates: `/data/ota/{mdb,dbc,mdb-boot,dbc-boot}/`
- Log bundles: `/data/log-bundles/logs-*.tar.gz`
- DBC files: `/data/dbc/`

## Dashboard Computer (DBC)

The service manages the Dashboard Computer connection:
- IP: `192.168.7.2`
- HTTP server: `192.168.7.1:31337`
- Enabled/disabled via `/usr/bin/keycard.sh`
- File transfers via SSH/SCP

## Logging

The service logs all operations including:
- Mode changes
- File transfers
- USB module loading/unloading
- DBC connectivity status
- Error conditions

## Security Notes

- SSH connections to DBC use `StrictHostKeyChecking=no`
- The service requires root access for kernel module operations
- Files are transferred with standard permissions (0644)

## License

This project is dual-licensed. The source code is available under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the Librescoot community
