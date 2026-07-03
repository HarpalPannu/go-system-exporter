# System Metrics Exporter

A standalone, high-performance, and lightweight system metrics exporter written in Go. It reads specific hardware and OS metrics directly from the Linux kernel and exposes them as a single JSON payload via a non-blocking HTTP GET endpoint `/api/system`. 

Designed specifically to be scraped periodically by a **Home Assistant RESTful sensor**.

## Features

- **Extremely Lightweight:** Compiled native binary with a memory footprint of ~6 MB and < 0.1% CPU usage.
- **Non-Blocking Rates:** Uses a background collector loop to calculate disk/network rates over time, ensuring HTTP GET responses return instantly.
- **Auto-Discovery:** Automatically detects your primary network interface on startup if not explicitly configured.
- **Resilient:** Gracefully handles missing metrics (like CPU temperature in VM/WSL environments) by returning `null` instead of crashing.
- **Home Assistant Friendly:** Formats the system boot time as an ISO 8601/RFC 3339 UTC timestamp, allowing Home Assistant to natively show relative uptime (e.g., *"3 hours ago"*).

---

## Installation & Setup

### 1. Prerequisites
Ensure you have Go installed on your Linux system (Go 1.21+ recommended).

### 2. Build the Exporter
Clone or copy the source files to your workspace directory and compile the binary:
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
  "network_interface": "eth0"
}
```
*Note: If `config.json` is missing or keys are omitted, the exporter defaults to port `8080` and auto-detects the active network interface.*

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

---

## API Payload Reference

A HTTP GET request to `http://<your-server-ip>:8080/api/system` returns:

```json
{
  "cpu_load": 2.7,
  "cpu_temp_c": null,
  "ram_available_mb": 7073,
  "uptime": "2026-07-03T13:45:47Z",
  "load_1m": 0.2,
  "load_5m": 0.41,
  "load_15m": 0.29,
  "disk_usage_percent": 0.2,
  "network_rx_mbps": 0.1,
  "network_tx_mbps": 0.3
}
```

---

## Home Assistant Installation (Custom Component Integration)

A custom component integration is provided in the `home_assistant/custom_components/system_exporter` directory. It asynchronously pulls the metrics from your Go system exporter and sets up individual sensors inside Home Assistant with full entity support, unique IDs (customizable in Lovelace UI), and correct icons/devices.

### 1. Installation

Copy the `system_exporter` directory into your Home Assistant's `custom_components` directory:

```bash
# From this workspace:
cp -r home_assistant/custom_components/system_exporter /path/to/your/homeassistant/config/custom_components/
```

### 2. Restart & Setup via UI

1. **Restart Home Assistant:** Restart your Home Assistant instance to load the custom component.
2. **Add integration from the UI:**
   - In Home Assistant, go to **Settings** -> **Devices & Services**.
   - Click the **Add Integration** button in the bottom right.
   - Search for **System Metrics Exporter** and select it.
   - Enter your Go Exporter Host URL (e.g. `http://192.168.1.50:8080`) and click **Submit**.
   
Home Assistant will connect to the API, verify the connection, and immediately register 10 sensors under a single integration instance:
- `sensor.system_cpu_load`
- `sensor.system_cpu_temperature`
- `sensor.system_ram_available`
- `sensor.system_uptime`
- `sensor.system_load_1m`
- `sensor.system_load_5m`
- `sensor.system_load_15m`
- `sensor.system_disk_usage`
- `sensor.system_network_rx_speed`
- `sensor.system_network_tx_speed`
