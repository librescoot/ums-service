# LibreScoot UMS Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

A Go service that manages USB gadget mode switching between network (g_ether) and USB Mass Storage (UMS) modes on embedded Linux devices. The service monitors Redis for mode change commands and handles file transfers between the host computer and the device.

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
# Switch to USB Mass Storage mode
redis-cli HSET usb mode ums
redis-cli PUBLISH usb mode

# Switch to normal (network) mode
redis-cli HSET usb mode normal
redis-cli PUBLISH usb mode
```

## USB Drive Structure

When in UMS mode, the virtual drive contains:

```
/
├── settings.toml        # Device settings (if exists)
├── wireguard/          # WireGuard VPN configs
│   └── *.conf
├── system-update/       # Place .mender files here
│   ├── librescoot-mdb-*.mender
│   └── librescoot-dbc-*.mender
└── maps/               # Place map files here
    ├── *.mbtiles
    └── *tiles.tar
```

## File Processing

### When switching to UMS mode:
1. Copies `/data/settings.toml` to USB drive (if exists)
2. Copies `/data/wireguard/*.conf` to USB `wireguard/` directory
3. Creates `system-update` and `maps` directories

### When switching to normal mode:
1. **Settings**: Copies settings.toml back and restarts settings-service
2. **WireGuard**: 
   - Syncs *.conf files from USB to `/data/wireguard/`
   - Removes local configs not present on USB
   - Restarts settings-service if any changes made
3. **Updates**: 
   - MDB updates: Installs locally and marks for reboot
   - DBC updates: Transfers to DBC and installs remotely
4. **Maps**: Transfers map files to DBC
5. Cleans the USB drive
6. Reboots if required by updates

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
- WireGuard configs: `/data/wireguard/`
- Updates: `/data/ota/`
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

This work is licensed under a
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png
[cc-by-nc-sa-shield]: https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-lightgrey.svg

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the LibreScoot community

