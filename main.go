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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
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
	RAMUsage                float64  `json:"ram_usage"`
	Uptime                  string   `json:"uptime"`
	DiskUsage               float64  `json:"disk_usage"`
	NetworkRxTotalMB        float64  `json:"network_rx_total_mb"`
	NetworkTxTotalMB        float64  `json:"network_tx_total_mb"`
	RpiUndervoltage         *bool    `json:"rpi_undervoltage"`
	RpiThrottled            *bool    `json:"rpi_throttled"`
	RpiUndervoltageOccurred *bool    `json:"rpi_undervoltage_has_occurred"`
	RpiThrottledOccurred    *bool    `json:"rpi_throttled_has_occurred"`
}

// Global thread-safe metrics storage for cached JSON.
// Pre-marshaling the JSON prevents the HTTP handler from holding the RLock
// during expensive reflection serialization, preventing lock contention
// under high concurrent request loads.
var (
	metricsMutex      sync.RWMutex
	globalMetricsJSON []byte
	globalMetrics     *SystemMetrics
	customRegistry    = prometheus.NewRegistry()
)

// roundToOne rounds a float64 value to one decimal place.
func roundToOne(val float64) float64 {
	return math.Round(val*10) / 10
}

// roundToTwo rounds a float64 value to two decimal places.
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
					log.Printf("Debug: Selected interface %s based on assigned address.", iface.Name)
					return iface.Name
				}
			}
		}
	}

	// Ultimate fallback: Just the first UP, non-loopback interface name
	if bestInterface == "" {
		for _, iface := range interfaces {
			if (iface.Flags&stdnet.FlagUp) != 0 && (iface.Flags&stdnet.FlagLoopback) == 0 {
				log.Printf("Debug: Selected fallback interface %s.", iface.Name)
				return iface.Name
			}
		}
	}

	if bestInterface != "" {
		log.Printf("Debug: Selected interface %s based on traffic volume.", bestInterface)
	}
	return bestInterface
}

// getCPUTemp retrieves CPU temperature and returns a pointer to the value, or nil if unavailable.
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
// Gracefully returns nils if the file cannot be read (e.g. on non-RPi systems).
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

func startMetricsCollector(netInterface string, isRaspberryPi bool) {
	// Warm up CPU stats — first call with interval 0 has no previous sample
	// and always returns 0%. This throwaway call primes the internal counters.
	cpu.Percent(0, false)

	ticker := time.NewTicker(1500 * time.Millisecond)
	go func() {
		// RPi sticky state kept goroutine-local (only this goroutine reads/writes them)
		// This strictly avoids any data race condition.
		var rpiUndervoltageSticky, rpiThrottledSticky bool

		for range ticker.C {
			// 1. CPU Load
			var cpuLoad float64
			cpuPercents, err := cpu.Percent(0, false)
			if err == nil && len(cpuPercents) > 0 {
				cpuLoad = roundToOne(cpuPercents[0])
			}


			// 2. CPU Temperature (graceful fail to nil/null)
			cpuTemp := getCPUTemp()

			// 3. RAM usage percent
			var ramUsage float64
			vmem, err := mem.VirtualMemory()
			if err == nil {
				ramUsage = roundToOne(vmem.UsedPercent)
			}

			// 4. Uptime (Boot time as ISO 8601/RFC 3339 timestamp)
			var uptimeStr string
			bootTime, err := host.BootTime()
			if err == nil {
				uptimeStr = time.Unix(int64(bootTime), 0).UTC().Format(time.RFC3339)
			}

			// 5. Load Averages (Removed per request, but we leave the blank space for formatting)

			// 6. Disk usage percent
			var diskUsage float64
			diskUsageStat, err := disk.Usage("/")
			if err == nil {
				diskUsage = roundToOne(diskUsageStat.UsedPercent)
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

			metrics := SystemMetrics{
				CPULoad:                 cpuLoad,
				CPUTempC:                cpuTemp,
				RAMUsage:                ramUsage,
				Uptime:                  uptimeStr,
				DiskUsage:               diskUsage,
				NetworkRxTotalMB:        networkRxTotalMB,
				NetworkTxTotalMB:        networkTxTotalMB,
				RpiUndervoltage:         rpiUV,
				RpiThrottled:            rpiThrottled,
				RpiUndervoltageOccurred: rpiUVOccurred,
				RpiThrottledOccurred:    rpiThrottledOccurred,
			}
			
			// Marshal JSON outside of the lock to prevent blocking API requests
			jsonData, err := json.MarshalIndent(metrics, "", "  ")
			if err == nil {
				// Only hold the lock briefly to swap the byte slice pointer
				metricsMutex.Lock()
				globalMetricsJSON = jsonData
				globalMetrics = &metrics
				metricsMutex.Unlock()
			}
		}
	}()
}

// systemCollector implements the prometheus.Collector interface.
type systemCollector struct{}

// Describe implements the prometheus.Collector interface.
func (c *systemCollector) Describe(ch chan<- *prometheus.Desc) {
	// Descriptions can be left empty for dynamic metrics
}

// Collect implements the prometheus.Collector interface.
func (c *systemCollector) Collect(ch chan<- prometheus.Metric) {
	metricsMutex.RLock()
	m := globalMetrics
	metricsMutex.RUnlock()

	if m == nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("system_cpu_load_percent", "CPU load percentage.", nil, nil),
		prometheus.GaugeValue,
		m.CPULoad,
	)

	if m.CPUTempC != nil {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_cpu_temp_celsius", "CPU temperature in degrees Celsius.", nil, nil),
			prometheus.GaugeValue,
			*m.CPUTempC,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("system_ram_usage_percent", "RAM usage percentage.", nil, nil),
		prometheus.GaugeValue,
		m.RAMUsage,
	)

	if t, err := time.Parse(time.RFC3339, m.Uptime); err == nil {
		bootTimeUnix := float64(t.Unix())
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_boot_time_seconds", "System boot time in unix epoch seconds.", nil, nil),
			prometheus.GaugeValue,
			bootTimeUnix,
		)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_uptime_seconds", "System uptime in seconds.", nil, nil),
			prometheus.GaugeValue,
			time.Since(t).Seconds(),
		)
	}

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("system_disk_usage_percent", "Disk usage percentage.", nil, nil),
		prometheus.GaugeValue,
		m.DiskUsage,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("system_network_rx_total_megabytes", "Total bytes received in Megabytes.", nil, nil),
		prometheus.GaugeValue,
		m.NetworkRxTotalMB,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("system_network_tx_total_megabytes", "Total bytes transmitted in Megabytes.", nil, nil),
		prometheus.GaugeValue,
		m.NetworkTxTotalMB,
	)

	if m.RpiUndervoltage != nil {
		val := 0.0
		if *m.RpiUndervoltage {
			val = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_rpi_undervoltage", "Raspberry Pi under-voltage detected (currently active).", nil, nil),
			prometheus.GaugeValue,
			val,
		)
	}

	if m.RpiThrottled != nil {
		val := 0.0
		if *m.RpiThrottled {
			val = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_rpi_throttled", "Raspberry Pi throttled (currently active).", nil, nil),
			prometheus.GaugeValue,
			val,
		)
	}

	if m.RpiUndervoltageOccurred != nil {
		val := 0.0
		if *m.RpiUndervoltageOccurred {
			val = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_rpi_undervoltage_has_occurred", "Raspberry Pi under-voltage occurred (previously active since last boot).", nil, nil),
			prometheus.GaugeValue,
			val,
		)
	}

	if m.RpiThrottledOccurred != nil {
		val := 0.0
		if *m.RpiThrottledOccurred {
			val = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("system_rpi_throttled_has_occurred", "Raspberry Pi throttled occurred (previously active since last boot).", nil, nil),
			prometheus.GaugeValue,
			val,
		)
	}
}

func init() {
	customRegistry.MustRegister(&systemCollector{})
}

// apiSystemHandler returns the cached JSON metrics payload.
func apiSystemHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	metricsMutex.RLock()
	data := globalMetricsJSON
	metricsMutex.RUnlock()

	if data == nil {
		http.Error(w, "Metrics initializing...", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}

func main() {
	log.Println("Starting System Metrics Exporter v2.0...")

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
	http.Handle("/metrics", promhttp.HandlerFor(customRegistry, promhttp.HandlerOpts{}))

	addr := fmt.Sprintf(":%d", config.Port)
	log.Printf("Server listening on http://localhost%s/api/system", addr)
	log.Printf("Prometheus metrics available on http://localhost%s/metrics", addr)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
