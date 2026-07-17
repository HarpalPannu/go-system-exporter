# System Metrics Exporter

> [!CAUTION]
> This is a personal hobby project. It is provided "as is" without any warranty, express or implied. The author takes no responsibility for system stability, security, or data integrity. Use at your own risk.

A standalone, high-performance, and lightweight system metrics exporter written in Go. It reads specific hardware and OS metrics directly from the Linux kernel and exposes them as a single JSON payload via a non-blocking HTTP GET endpoint `/api/system`. 

Designed specifically to be scraped periodically by a **Home Assistant RESTful sensor**.

## Features

- **Extremely Lightweight:** Compiled native binary with a memory footprint of ~6 MB and < 0.1% CPU usage.
- **Non-Blocking Rates:** Uses a background collector loop to calculate disk/network rates over time, ensuring HTTP GET responses return instantly.
- **Auto-Discovery:** Automatically detects your primary network interface on startup if not explicitly configured.
- **Resilient:** Gracefully handles missing metrics (like CPU temperature in VM/WSL environments) by returning `null` instead of crashing.
- **Home Assistant Friendly:** Formats the system boot time as an ISO 8601/RFC 3339 UTC timestamp, allowing Home Assistant to natively show relative uptime (e.g., *"3 hours ago"*).
- **Prometheus Support:** Exports dynamic system metrics natively in Prometheus format at the `/metrics` endpoint.

---

## Installation & Setup

### Option A: Use a Precompiled Binary (Recommended)
You can download a precompiled binary for your system architecture directly from the `bin/` folder of this repository:

- 💻 **[Intel/AMD 64-bit (x86_64)](https://github.com/HarpalPannu/go-system-exporter/blob/main/bin/system_exporter_linux_amd64)** (Standard PC, Server, VM)
- 🍓 **[ARM 64-bit (arm64)](https://github.com/HarpalPannu/go-system-exporter/blob/main/bin/system_exporter_linux_arm64)** (Raspberry Pi 4/5, AWS Graviton)

Download the binary to your server, rename it to `system_exporter`, and make it executable:
```bash
chmod +x system_exporter
```

---

### Option B: Build from Source
If you prefer to compile from source:

1. **Prerequisites:** Ensure you have Go installed on your Linux system (Go 1.21+ recommended).
2. **Build the Exporter:**
   ```bash
   # Initialize and fetch dependencies
   go mod tidy

   # Build the optimized executable
   go build -ldflags="-s -w" -o system_exporter main.go
   ```
   *The `-ldflags="-s -w"` flags strip debugging information, reducing the binary size.*

### 3. Configuration (`config.json`)
Create a `config.json` file in the same directory as the executable:
```json
{
  "port": 8080,
  "network_interface": "eth0",
  "is_raspberry_pi": false
}
```
*Note: If `config.json` is missing or keys are omitted, the exporter defaults to port `8080` and auto-detects the active network interface. Set `"is_raspberry_pi": true` to enable Raspberry Pi specific power supply (under-voltage) and throttling checks.*

---

## Running as a systemd Daemon

To ensure the exporter runs continuously in the background and restarts automatically on system boot:

1. Move the binary and config file to `/opt/system_exporter`:
   ```bash
   sudo mkdir -p /opt/system_exporter
   sudo cp system_exporter config.json /opt/system_exporter/
   ```

2. Create the systemd service unit file `/etc/systemd/system/system-exporter.service`:
   ```ini
   [Unit]
   Description=Lightweight System Metrics Exporter
   After=network.target

   [Service]
   Type=simple
   User=root
   WorkingDirectory=/opt/system_exporter
   ExecStart=/opt/system_exporter/system_exporter
   Restart=always
   RestartSec=5
   StandardOutput=journal
   StandardError=journal

   [Install]
   WantedBy=multi-user.target
   ```

3. Enable and start the service:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable system-exporter.service
   sudo systemctl start system-exporter.service
   ```

4. Verify it is running:
   ```bash
   sudo systemctl status system-exporter.service
   ```

### Updating

To easily update your installation to the latest binary from GitHub, simply run the included update script (requires the systemd setup above):
```bash
sudo ./update.sh
```

---

## API Payload Reference

A HTTP GET request to `http://<your-server-ip>:8080/api/system` returns:

```json
{
  "cpu_load": 2.7,
  "cpu_temp_c": null,
  "ram_usage": 45.2,
  "uptime": "2026-07-03T13:45:47Z",
  "disk_usage": 72.5,
  "network_rx_total_mb": 150.1,
  "network_tx_total_mb": 35.3,
  "rpi_undervoltage": null,
  "rpi_throttled": null,
  "rpi_undervoltage_has_occurred": null,
  "rpi_throttled_has_occurred": null
}
```

---

## Prometheus Metrics

A HTTP GET request to `http://<your-server-ip>:8080/metrics` exposes standard Prometheus gauges natively. The exporter automatically detects available hardware and omits missing sensors to prevent false zeros:

- `system_cpu_load_percent`
- `system_cpu_temp_celsius` *(omitted if missing)*
- `system_ram_usage_percent`
- `system_boot_time_seconds`
- `system_uptime_seconds`
- `system_disk_usage_percent`
- `system_network_rx_total_megabytes`
- `system_network_tx_total_megabytes`
- Raspberry Pi specific throttling and under-voltage gauges *(omitted if not on RPi)*

---

## Home Assistant Integration

You can natively integrate these metrics into Home Assistant using the dedicated HACS custom component.

For installation and setup instructions, please refer to the integration repository:
👉 **[HarpalPannu/ha-system-exporter](https://github.com/HarpalPannu/ha-system-exporter)**
