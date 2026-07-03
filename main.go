package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	stdnet "net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	netops "github.com/shirou/gopsutil/v3/net"
)

// Config represents the server configuration file structure.
type Config struct {
	Port             int    `json:"port"`
	NetworkInterface string `json:"network_interface"`
	IsRaspberryPi    bool   `json:"is_raspberry_pi"`
}

// SystemMetrics defines the required JSON output structure.
// Using pointers for nullable/resilient fields such as CPUTempC.
type SystemMetrics struct {
	CPULoad                 float64  `json:"cpu_load"`
	CPUTempC                *float64 `json:"cpu_temp_c"`
	RAMAvailableMB          float64  `json:"ram_available_mb"`
	RAMTotalMB              float64  `json:"ram_total_mb"`
	Uptime                  string   `json:"uptime"`
	Load1m                  float64  `json:"load_1m"`
	Load5m                  float64  `json:"load_5m"`
	Load15m                 float64  `json:"load_15m"`
	DiskUsagePercent        float64  `json:"disk_usage_percent"`
	DiskTotalGB             float64  `json:"disk_total_gb"`
	NetworkRxTotalMB        float64  `json:"network_rx_total_mb"`
	NetworkTxTotalMB        float64  `json:"network_tx_total_mb"`
	RpiUndervoltage         *bool    `json:"rpi_undervoltage"`
	RpiThrottled            *bool    `json:"rpi_throttled"`
	RpiUndervoltageOccurred *bool    `json:"rpi_undervoltage_has_occurred"`
	RpiThrottledOccurred    *bool    `json:"rpi_throttled_has_occurred"`
}

// Global thread-safe metrics storage
var (
	metricsMutex            sync.RWMutex
	globalMetrics           SystemMetrics
	rpiUndervoltageSticky   bool
	rpiThrottledSticky      bool
)

// Helper to round float64 values to one decimal place.
func roundToOne(val float64) float64 {
	return math.Round(val*10) / 10
}

// Helper to round float64 values to two decimal places.
func roundToTwo(val float64) float64 {
	return math.Round(val*100) / 100
}

// detectPrimaryInterface scans local UP network interfaces and returns the name
// of the one with the highest historical traffic, defaulting to the first active non-loopback interface.
func detectPrimaryInterface() string {
	interfaces, err := stdnet.Interfaces()
	if err != nil {
		log.Printf("Warning: Failed to retrieve network interfaces: %v", err)
		return ""
	}

	var bestInterface string
	var maxBytes uint64

	// Get net IO counters for all interfaces to find active traffic
	counters, err := netops.IOCounters(true)
	if err == nil {
		for _, c := range counters {
			// Skip loopback interfaces
			if c.Name == "lo" || strings.HasPrefix(c.Name, "loop") {
				continue
			}
			// Match counter names against standard system interfaces that are UP
			for _, iface := range interfaces {
				if iface.Name == c.Name {
					if (iface.Flags&stdnet.FlagUp) != 0 && (iface.Flags&stdnet.FlagLoopback) == 0 {
						totalBytes := c.BytesRecv + c.BytesSent
						if totalBytes > maxBytes {
							maxBytes = totalBytes
							bestInterface = c.Name
						}
					}
				}
			}
		}
	}

	// Fallback: Pick the first UP, non-loopback interface with an assigned address
	if bestInterface == "" {
		for _, iface := range interfaces {
			if (iface.Flags&stdnet.FlagUp) != 0 && (iface.Flags&stdnet.FlagLoopback) == 0 {
				addrs, err := iface.Addrs()
				if err == nil && len(addrs) > 0 {
					return iface.Name
				}
			}
		}
	}

	// Ultimate fallback: Just the first UP, non-loopback interface name
	if bestInterface == "" {
		for _, iface := range interfaces {
			if (iface.Flags&stdnet.FlagUp) != 0 && (iface.Flags&stdnet.FlagLoopback) == 0 {
				return iface.Name
			}
		}
	}

	return bestInterface
}

// getCPUTemp retrieves CPU temperature and returns a pointer to the value, or nil if unavailable
func getCPUTemp() *float64 {
	temps, err := host.SensorsTemperatures()
	if err != nil || len(temps) == 0 {
		return nil
	}

	// Look for common CPU temperature sensors (e.g., coretemp, k10temp, cpu_thermal)
	for _, t := range temps {
		k := strings.ToLower(t.SensorKey)
		if strings.Contains(k, "cpu") || strings.Contains(k, "core") || strings.Contains(k, "temp") {
			val := roundToOne(t.Temperature)
			return &val
		}
	}

	// Fallback to first available sensor if any
	val := roundToOne(temps[0].Temperature)
	return &val
}

// getRpiThrottledState reads the Raspberry Pi firmware get_throttled sysfs node.
// Returns under-voltage, throttled, under-voltage occurred, and throttled occurred states.
func getRpiThrottledState() (*bool, *bool, *bool, *bool) {
	const throttledPath = "/sys/devices/platform/soc/soc:firmware/get_throttled"
	data, err := os.ReadFile(throttledPath)
	if err != nil {
		return nil, nil, nil, nil
	}

	content := strings.TrimSpace(string(data))
	content = strings.TrimPrefix(content, "0x")
	content = strings.TrimPrefix(content, "0X")

	// Parse as hexadecimal (base 16) by default as the sysfs value is formatted in hex (e.g. "50000").
	val, err := strconv.ParseUint(content, 16, 64)
	if err != nil {
		// Fallback to base 10
		val, err = strconv.ParseUint(content, 10, 64)
		if err != nil {
			return nil, nil, nil, nil
		}
	}

	// Bit 0: Under-voltage detected (currently active)
	underVoltage := (val & 0x1) != 0
	// Bit 2: Throttled (currently active)
	throttled := (val & 0x4) != 0
	// Bit 16: Under-voltage occurred (previously active since last boot)
	underVoltageOccurred := (val & 0x10000) != 0
	// Bit 18: Throttled occurred (previously active since last boot)
	throttledOccurred := (val & 0x40000) != 0

	return &underVoltage, &throttled, &underVoltageOccurred, &throttledOccurred
}

// startMetricsCollector initiates the background goroutine to gather and calculate metrics
func startMetricsCollector(netInterface string, isRaspberryPi bool) {
	ticker := time.NewTicker(1500 * time.Millisecond)
	go func() {
		for range ticker.C {
			// 1. CPU Load
			var cpuLoad float64
			cpuPercents, err := cpu.Percent(0, false)
			if err == nil && len(cpuPercents) > 0 {
				cpuLoad = roundToOne(cpuPercents[0])
			}

			// 2. CPU Temperature (graceful fail to nil/null)
			cpuTemp := getCPUTemp()

			// 3. RAM available and total in MB
			var ramAvailableMB, ramTotalMB float64
			vmem, err := mem.VirtualMemory()
			if err == nil {
				ramAvailableMB = math.Round(float64(vmem.Available) / (1024 * 1024))
				ramTotalMB = math.Round(float64(vmem.Total) / (1024 * 1024))
			}

			// 4. Uptime (Boot time as ISO 8601/RFC 3339 timestamp)
			var uptimeStr string
			bootTime, err := host.BootTime()
			if err == nil {
				uptimeStr = time.Unix(int64(bootTime), 0).UTC().Format(time.RFC3339)
			}

			// 5. Load Averages (1m, 5m, 15m)
			var load1m, load5m, load15m float64
			avg, err := load.Avg()
			if err == nil {
				load1m = roundToTwo(avg.Load1)
				load5m = roundToTwo(avg.Load5)
				load15m = roundToTwo(avg.Load15)
			}

			// 6. Disk Usage Percentage and Total size
			var diskUsagePercent, diskTotalGB float64
			diskUsage, err := disk.Usage("/")
			if err == nil {
				diskUsagePercent = roundToOne(diskUsage.UsedPercent)
				diskTotalGB = roundToOne(float64(diskUsage.Total) / (1024 * 1024 * 1024))
			}

			// 7. Network Bytes
			var currentNetRx, currentNetTx uint64
			netIO, err := netops.IOCounters(true)
			if err == nil {
				for _, c := range netIO {
					if c.Name == netInterface {
						currentNetRx = c.BytesRecv
						currentNetTx = c.BytesSent
						break
					}
				}
			}

			// Convert to Megabytes
			networkRxTotalMB := roundToOne(float64(currentNetRx) / (1024 * 1024))
			networkTxTotalMB := roundToOne(float64(currentNetTx) / (1024 * 1024))

			// RPi specific power and throttling checks (resilient fallback to nil)
			var rpiUV, rpiThrottled, rpiUVOccurred, rpiThrottledOccurred *bool
			if isRaspberryPi {
				uv, th, uvOcc, thOcc := getRpiThrottledState()
				if uv != nil {
					rpiUV = uv
					rpiThrottled = th
					
					// Update local sticky cache (never reset to false)
					if *uvOcc {
						rpiUndervoltageSticky = true
					}
					if *thOcc {
						rpiThrottledSticky = true
					}
					
					// Set local sticky references
					uvOccVal := rpiUndervoltageSticky
					thOccVal := rpiThrottledSticky
					rpiUVOccurred = &uvOccVal
					rpiThrottledOccurred = &thOccVal
				}
			}

			// Update the thread-safe global structure
			metricsMutex.Lock()
			globalMetrics = SystemMetrics{
				CPULoad:                 cpuLoad,
				CPUTempC:                cpuTemp,
				RAMAvailableMB:          ramAvailableMB,
				RAMTotalMB:              ramTotalMB,
				Uptime:                  uptimeStr,
				Load1m:                  load1m,
				Load5m:                  load5m,
				Load15m:                 load15m,
				DiskUsagePercent:        diskUsagePercent,
				DiskTotalGB:             diskTotalGB,
				NetworkRxTotalMB:        networkRxTotalMB,
				NetworkTxTotalMB:        networkTxTotalMB,
				RpiUndervoltage:         rpiUV,
				RpiThrottled:            rpiThrottled,
				RpiUndervoltageOccurred: rpiUVOccurred,
				RpiThrottledOccurred:    rpiThrottledOccurred,
			}
			metricsMutex.Unlock()
		}
	}()
}

// apiSystemHandler returns the thread-safe global metrics as a JSON payload
func apiSystemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	metricsMutex.RLock()
	data, err := json.MarshalIndent(globalMetrics, "", "  ")
	metricsMutex.RUnlock()

	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func main() {
	log.Println("Starting System Metrics Exporter...")

	// 1. Locate and parse configuration file
	exePath, err := os.Executable()
	var exeDir string
	if err != nil {
		exeDir = "."
	} else {
		exeDir = filepath.Dir(exePath)
	}

	configPath := filepath.Join(exeDir, "config.json")
	var config Config

	configFile, err := os.Open(configPath)
	if err != nil {
		log.Printf("Warning: config.json not found in %s. Using default configurations.", exeDir)
		config.Port = 8080
	} else {
		defer configFile.Close()
		decoder := json.NewDecoder(configFile)
		if err := decoder.Decode(&config); err != nil {
			log.Printf("Warning: Failed to parse config.json (%v). Falling back to defaults.", err)
			config.Port = 8080
		}
	}

	// 2. Validate configuration and set defaults
	if config.Port <= 0 {
		config.Port = 8080
	}

	if config.NetworkInterface == "" {
		config.NetworkInterface = detectPrimaryInterface()
		log.Printf("Auto-detected primary network interface: %s", config.NetworkInterface)
	} else {
		log.Printf("Configured network interface: %s", config.NetworkInterface)
	}

	// Log Raspberry Pi configuration
	if config.IsRaspberryPi {
		log.Println("Raspberry Pi mode enabled (power/throttling checks active)")
	} else {
		log.Println("Raspberry Pi mode disabled")
	}

	// 3. Start background collector goroutine
	startMetricsCollector(config.NetworkInterface, config.IsRaspberryPi)

	// 4. Register HTTP endpoint and start server
	http.HandleFunc("/api/system", apiSystemHandler)

	addr := fmt.Sprintf(":%d", config.Port)
	log.Printf("Server listening on http://localhost%s/api/system", addr)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
