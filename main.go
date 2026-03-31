package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
	"tinygo.org/x/bluetooth"
)

var (
	// Shared BLE adapter instance.
	bleAdapter = bluetooth.DefaultAdapter

	// Ensure we only call Enable() once for the lifetime of the process.
	bleEnableOnce sync.Once
	bleEnableErr  error

	// MQTT client used for future publish/subscribe endpoints.
	mqttClient mqtt.Client

	// Expected payload format from device: HH:MM:SS.hh (hundredths).
	stopwatchDataRe = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\.\d{2}$`)

	// In-memory realtime stream hub for frontend clients (SSE).
	realtimeHub = struct {
		mu      sync.RWMutex
		latest  map[string]StopwatchRealtimeEvent
		clients map[chan StopwatchRealtimeEvent]struct{}
	}{
		latest:  make(map[string]StopwatchRealtimeEvent),
		clients: make(map[chan StopwatchRealtimeEvent]struct{}),
	}
)

func ensureBLEAdapterEnabled() error {
	bleEnableOnce.Do(func() {
		bleEnableErr = bleAdapter.Enable()
	})
	return bleEnableErr
}

// Config holds application configuration (similar to the Python Settings model).
type Config struct {
	AppName          string
	Debug            bool
	Port             int
	BLENamePrefix    string
	MockBLE          bool
	WiFiServiceUUID  string
	WiFiConfigChar   string
	WiFiStatusChar   string

	// MQTT broker configuration (mirrors Python backend defaults where possible).
	MQTTEnabled             bool
	MQTTBrokerHost         string
	MQTTBrokerPort         int
	MQTTUsername           string
	MQTTPassword           string
	// MQTT publish/subscribe topics.
	// These are not used yet by HTTP endpoints, but they are exposed in /api/config
	// so the frontend can confirm they are loaded.
	//
	// Publish topic supports a placeholder `{device_id}` for future use.
	MQTTPublishTopic      string
	MQTTSubscribeTopic    string
	MQTTClientID           string
	MQTTConnectTimeoutSec  int
}

// DeviceSummary mirrors app.backend.app.models.DeviceSummary.
type DeviceSummary struct {
	ID   string  `json:"id"`
	Name *string `json:"name,omitempty"`
	RSSI *int    `json:"rssi,omitempty"`
}

// WifiConfig mirrors app.backend.app.models.WifiConfig.
// Accepts either "password" or "pass" in JSON; the firmware expects "pass" in the BLE payload.
type WifiConfig struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
	Pass     string `json:"pass"`
}

// WifiProvisionResult mirrors app.backend.app.models.WifiProvisionResult.
type WifiProvisionResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// BatchWifiProvisionRequest configures multiple devices with one request.
// A shared WiFi config is applied to each device in sequence.
type BatchWifiProvisionRequest struct {
	DeviceIDs []string `json:"device_ids"`
	SSID      string   `json:"ssid"`
	Password  string   `json:"password"`
	Pass      string   `json:"pass"`
}

type BatchWifiProvisionItem struct {
	DeviceID string `json:"device_id"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
}

type BatchWifiProvisionResponse struct {
	Total   int                      `json:"total"`
	Success int                      `json:"success"`
	Failed  int                      `json:"failed"`
	Results []BatchWifiProvisionItem `json:"results"`
}

// HealthStatus mirrors app.backend.app.models.HealthStatus.
type HealthStatus struct {
	Status string `json:"status"`
}

// StopwatchRealtimeEvent is emitted whenever MQTT receives stopwatch/{device_id}/data.
type StopwatchRealtimeEvent struct {
	DeviceID   string    `json:"device_id"`
	Data       string    `json:"data"`
	Status     string    `json:"status,omitempty"`
	Topic      string    `json:"topic"`
	ReceivedAt time.Time `json:"received_at"`
}

type mqttStopwatchPayload struct {
	Data   string `json:"data"`
	Status string `json:"status"`
}

// BLEService abstracts BLE operations so we can plug in
// either a mock implementation or a real one backed by a
// BLE library.
type BLEService interface {
	DiscoverDevices(ctx context.Context) ([]DeviceSummary, error)
	ConfigureWiFi(ctx context.Context, deviceID string, cfg WifiConfig) (WifiProvisionResult, error)
}

// mockBLEService provides the same fake behavior as the Python MOCK_BLE mode.
type mockBLEService struct {
	cfg *Config
}

func (m *mockBLEService) DiscoverDevices(ctx context.Context) ([]DeviceSummary, error) {
	log.Println("MOCK_BLE enabled - returning fake devices")

	name1 := fmt.Sprintf("%sMock-1", m.cfg.BLENamePrefix)
	name2 := fmt.Sprintf("%sMock-2", m.cfg.BLENamePrefix)
	rssi1 := -42
	rssi2 := -55

	return []DeviceSummary{
		{ID: "FAKE:01", Name: &name1, RSSI: &rssi1},
		{ID: "FAKE:02", Name: &name2, RSSI: &rssi2},
	}, nil
}

func (m *mockBLEService) ConfigureWiFi(ctx context.Context, deviceID string, cfg WifiConfig) (WifiProvisionResult, error) {
	log.Printf("MOCK_BLE enabled - pretending to configure WiFi for %s (SSID=%s)\n", deviceID, cfg.SSID)
	payload, _ := json.Marshal(map[string]any{
		"ssid": cfg.SSID,
		"pass": "***",
	})
	log.Printf("Would send over BLE: %s\n", string(payload))
	select {
	case <-time.After(200 * time.Millisecond):
		return WifiProvisionResult{
			Success: true,
			Message: "Mock provisioning succeeded",
		}, nil
	case <-ctx.Done():
		return WifiProvisionResult{
			Success: false,
			Message: "Context cancelled",
		}, ctx.Err()
	}
}

// realBLEService is a placeholder for a real BLE implementation.
// You can wire this up using a Go BLE library such as github.com/go-ble/ble
// to mirror the behavior in app/backend/app/ble_service.py.
type realBLEService struct {
	cfg *Config
}

func (r *realBLEService) DiscoverDevices(ctx context.Context) ([]DeviceSummary, error) {
	if err := ensureBLEAdapterEnabled(); err != nil {
		return nil, fmt.Errorf("enable BLE adapter: %w", err)
	}

	adapter := bleAdapter

	resultsByAddr := make(map[string]DeviceSummary)
	var mu sync.Mutex

	// Stop scanning when the context is done (timeout or cancellation).
	go func() {
		<-ctx.Done()
		_ = adapter.StopScan()
	}()

	log.Printf("Starting BLE scan for devices with prefix %q", r.cfg.BLENamePrefix)

	err := adapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
		name := res.LocalName()
		if name == "" {
			return
		}
		if !strings.HasPrefix(name, r.cfg.BLENamePrefix) {
			return
		}

		addr := res.Address.String()
		rssi := int(res.RSSI)
		nameCopy := name

		mu.Lock()
		resultsByAddr[addr] = DeviceSummary{
			ID:   addr,
			Name: &nameCopy,
			RSSI: &rssi,
		}
		mu.Unlock()
	})

	// If the context was cancelled or timed out, treat that as a normal stop.
	if ctx.Err() != nil {
		log.Printf("BLE scan stopped due to context: %v", ctx.Err())
	} else if err != nil {
		return nil, fmt.Errorf("BLE scan error: %w", err)
	}

	mu.Lock()
	defer mu.Unlock()

	out := make([]DeviceSummary, 0, len(resultsByAddr))
	for _, d := range resultsByAddr {
		out = append(out, d)
	}

	log.Printf("Found %d matching BLE devices", len(out))
	return out, nil
}

func (r *realBLEService) ConfigureWiFi(ctx context.Context, deviceID string, cfg WifiConfig) (WifiProvisionResult, error) {
	if err := ensureBLEAdapterEnabled(); err != nil {
		return WifiProvisionResult{
			Success: false,
			Message: fmt.Sprintf("enable BLE adapter: %v", err),
		}, fmt.Errorf("enable BLE adapter: %w", err)
	}

	adapter := bleAdapter

	// Normalize the target ID to the same format we use when scanning.
	targetID := strings.TrimSpace(strings.ToUpper(deviceID))

	// Re-scan briefly to find the device and obtain its Address handle.
	resultCh := make(chan bluetooth.ScanResult, 1)

	log.Printf("Running short BLE scan to find device %s", targetID)

	scanErr := adapter.Scan(func(_ *bluetooth.Adapter, res bluetooth.ScanResult) {
		addrStr := strings.ToUpper(strings.TrimSpace(res.Address.String()))
		if addrStr == targetID {
			log.Printf("Matched device %s during provisioning scan", addrStr)
			_ = adapter.StopScan()
			select {
			case resultCh <- res:
			default:
			}
		}
	})
	if scanErr != nil {
		msg := fmt.Sprintf("BLE scan error before connect: %v", scanErr)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, scanErr
	}

	var scanRes bluetooth.ScanResult
	select {
	case <-ctx.Done():
		msg := "device not seen in scan before connect (context cancelled or timed out)"
		log.Println(msg)
		_ = adapter.StopScan()
		return WifiProvisionResult{Success: false, Message: msg}, ctx.Err()
	case scanRes = <-resultCh:
		// Proceed with connect.
	}

	log.Printf("Connecting to BLE device %s", scanRes.Address.String())

	device, err := adapter.Connect(scanRes.Address, bluetooth.ConnectionParams{})
	if err != nil {
		msg := fmt.Sprintf("failed to connect to device %s: %v", scanRes.Address.String(), err)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, err
	}
	defer func() {
		if derr := device.Disconnect(); derr != nil {
			log.Printf("error disconnecting from device: %v", derr)
		}
	}()

	// Discover services and characteristics.
	services, err := device.DiscoverServices(nil)
	if err != nil {
		msg := fmt.Sprintf("discover services failed: %v", err)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, err
	}

	expectedService := strings.ToLower(r.cfg.WiFiServiceUUID)
	configCharUUID := strings.ToLower(r.cfg.WiFiConfigChar)
	statusCharUUID := strings.ToLower(r.cfg.WiFiStatusChar)

	var (
		configChar bluetooth.DeviceCharacteristic
		statusChar *bluetooth.DeviceCharacteristic
		foundCfg   bool
	)

	for _, svc := range services {
		if strings.ToLower(svc.UUID().String()) != expectedService {
			continue
		}
		log.Printf("Found WiFi provisioning service %s", svc.UUID().String())

		chars, err := svc.DiscoverCharacteristics(nil)
		if err != nil {
			msg := fmt.Sprintf("discover characteristics failed: %v", err)
			log.Println(msg)
			return WifiProvisionResult{Success: false, Message: msg}, err
		}

		for _, ch := range chars {
			u := strings.ToLower(ch.UUID().String())
			switch u {
			case configCharUUID:
				cc := ch
				configChar = cc
				foundCfg = true
			case statusCharUUID:
				sc := ch
				statusChar = &sc
			}
		}
	}

	if !foundCfg {
		msg := "WiFi provisioning config characteristic not found on device"
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, nil
	}

	// Prepare JSON payload: {"ssid": "...", "pass": "..."}. Use either password or pass from request.
	pass := cfg.Password
	if pass == "" {
		pass = cfg.Pass
	}
	payload, err := json.Marshal(map[string]any{
		"ssid": cfg.SSID,
		"pass": pass,
	})
	if err != nil {
		msg := fmt.Sprintf("failed to marshal WiFi payload: %v", err)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, err
	}

	// Subscribe to notifications/indications on the status characteristic if present,
	// otherwise fall back to the config characteristic. The ESP32 firmware should send
	// the IP address as an indication on one of these.
	notifChar := configChar
	if statusChar != nil {
		notifChar = *statusChar
	}

	ipCh := make(chan string, 1)

	if err := notifChar.EnableNotifications(func(buf []byte) {
		s := strings.TrimSpace(string(buf))
		if s == "" {
			return
		}
		select {
		case ipCh <- s:
		default:
		}
	}); err != nil {
		msg := fmt.Sprintf("failed to enable notifications: %v", err)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, err
	}

	log.Printf("Writing WiFi credentials to characteristic %s (len=%d)", configChar.UUID().String(), len(payload))

	// WriteWithoutResponse is the cross-platform write command supported by this library.
	if _, err := configChar.WriteWithoutResponse(payload); err != nil {
		msg := fmt.Sprintf("failed to write WiFi credentials: %v", err)
		log.Println(msg)
		return WifiProvisionResult{Success: false, Message: msg}, err
	}

	log.Println("WiFi credentials written, waiting for IP indication / status")

	// Wait for an indication carrying the IP address string, or time out.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case ip := <-ipCh:
		ip = strings.TrimSpace(ip)
		log.Printf("Received status/indication from device: %s", ip)
		return WifiProvisionResult{
			Success: true,
			Message: ip,
		}, nil
	case <-ctx.Done():
		msg := "context done while waiting for device IP response"
		log.Println(msg)
		return WifiProvisionResult{
			Success: true,
			Message: "Provisioning command sent (no indication response)",
		}, ctx.Err()
	case <-timer.C:
		msg := "no indication response from device before timeout"
		log.Println(msg)
		return WifiProvisionResult{
			Success: true,
			Message: "Provisioning command sent (no indication response)",
		}, nil
	}
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	// Step 1: connect to MQTT as soon as the server starts up.
	// We do not block the HTTP server on MQTT connection; failures are logged.
	mqttClient = connectMQTT(logger, cfg)

	var bleSvc BLEService
	if cfg.MockBLE {
		bleSvc = &mockBLEService{cfg: cfg}
	} else {
		bleSvc = &realBLEService{cfg: cfg}
	}

	mux := http.NewServeMux()

	// GET /health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, HealthStatus{Status: "ok"})
	})

	// GET /api/config
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, configForDisplay(cfg))
	})

	// GET /api/devices
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		devices, err := bleSvc.DiscoverDevices(ctx)
		if err != nil {
			log.Printf("discover devices failed: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, devices)
	})

	// POST /api/devices/wifi/batch
	// Provision multiple devices in sequence using the same WiFi credentials.
	mux.HandleFunc("/api/devices/wifi/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		var req BatchWifiProvisionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}

		ssid := strings.TrimSpace(req.SSID)
		if ssid == "" {
			http.Error(w, "ssid is required", http.StatusBadRequest)
			return
		}
		if len(req.DeviceIDs) == 0 {
			http.Error(w, "device_ids is required", http.StatusBadRequest)
			return
		}

		// Apply one shared config to each device.
		sharedCfg := WifiConfig{
			SSID:     ssid,
			Password: req.Password,
			Pass:     req.Pass,
		}

		resp := BatchWifiProvisionResponse{
			Results: make([]BatchWifiProvisionItem, 0, len(req.DeviceIDs)),
		}

		seen := make(map[string]struct{}, len(req.DeviceIDs))
		for _, rawID := range req.DeviceIDs {
			deviceID := strings.TrimSpace(rawID)
			if deviceID == "" {
				continue
			}
			if _, ok := seen[deviceID]; ok {
				continue
			}
			seen[deviceID] = struct{}{}

			resp.Total++
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			result, err := bleSvc.ConfigureWiFi(ctx, deviceID, sharedCfg)
			cancel()

			item := BatchWifiProvisionItem{
				DeviceID: deviceID,
				Success:  result.Success,
				Message:  result.Message,
			}
			if err != nil {
				log.Printf("batch configure wifi failed for %s: %v", deviceID, err)
				if strings.TrimSpace(item.Message) == "" {
					item.Message = err.Error()
				}
			}

			if item.Success {
				resp.Success++
			} else {
				resp.Failed++
			}
			resp.Results = append(resp.Results, item)
		}

		if resp.Total == 0 {
			http.Error(w, "no valid device_ids provided", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// POST /api/devices/{device_id}/wifi
	mux.HandleFunc("/api/devices/", func(w http.ResponseWriter, r *http.Request) {
		// Only handle paths like /api/devices/{id}/wifi
		if !strings.HasPrefix(r.URL.Path, "/api/devices/") {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/wifi") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		// Extract device_id between the prefix and suffix.
		path := strings.TrimPrefix(r.URL.Path, "/api/devices/")
		path = strings.TrimSuffix(path, "/wifi")
		path = strings.TrimSuffix(path, "/")
		deviceID := path
		if deviceID == "" {
			http.Error(w, "missing device_id in path", http.StatusBadRequest)
			return
		}

		var bodyCfg WifiConfig
		if err := json.NewDecoder(r.Body).Decode(&bodyCfg); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(bodyCfg.SSID) == "" {
			http.Error(w, "ssid is required", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		res, err := bleSvc.ConfigureWiFi(ctx, deviceID, bodyCfg)
		if err != nil {
			log.Printf("configure wifi failed: %v", err)
		}
		writeJSON(w, http.StatusOK, res)
	})

	// GET /api/mqtt/stream
	// Server-Sent Events stream for frontend real-time stopwatch display.
	mux.HandleFunc("/api/mqtt/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Buffered channel so brief frontend hiccups do not block MQTT callback.
		ch := make(chan StopwatchRealtimeEvent, 256)
		registerRealtimeClient(ch)
		defer unregisterRealtimeClient(ch)

		// Send a tiny ready event so frontend knows stream is open.
		_, _ = fmt.Fprint(w, "event: ready\ndata: ok\n\n")
		flusher.Flush()

		// Optionally send snapshot first (latest item per known device).
		for _, ev := range latestRealtimeSnapshot() {
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: stopwatch\ndata: %s\n\n", b)
			flusher.Flush()
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				b, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "event: stopwatch\ndata: %s\n\n", b)
				flusher.Flush()
			}
		}
	})

	// POST /api/mqtt/devices/{device_id}/{command}
	// Publishes command payload to stopwatch/{device_id}/cmd (template from MQTT_PUBLISH_TOPIC).
	// Supported commands: start, stop, reset.
	mux.HandleFunc("/api/mqtt/devices/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/mqtt/devices/") {
			http.NotFound(w, r)
			return
		}
		if !(strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/reset")) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/api/mqtt/devices/")
		path = strings.TrimSuffix(path, "/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			http.Error(w, "invalid mqtt command path", http.StatusBadRequest)
			return
		}
		deviceID := strings.TrimSpace(parts[0])
		command := strings.TrimSpace(parts[1])
		if deviceID == "" {
			http.Error(w, "missing device_id in path", http.StatusBadRequest)
			return
		}
		if command != "start" && command != "stop" && command != "reset" {
			http.Error(w, "unsupported command", http.StatusBadRequest)
			return
		}

		topic := buildPublishTopic(cfg.MQTTPublishTopic, deviceID)
		if err := publishMQTTMessage(topic, command); err != nil {
			log.Printf("mqtt publish failed (topic=%s): %v", topic, err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"success":   true,
			"device_id": deviceID,
			"topic":     topic,
			"message":   command,
		})
	})

	// Simple static frontend for real-time stopwatch display.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "web/index.html")
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	logger.Printf("Starting Go backend on %s (app_name=%s, mock_ble=%v)", addr, cfg.AppName, cfg.MockBLE)

	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Fatalf("server error: %v", err)
	}
}

func loadConfig() (*Config, error) {
	// Try to load a .env file sitting next to this Go app (optional).
	_ = godotenv.Load()

	cfg := &Config{
		AppName:         getEnv("APP_NAME", "ESP32 Backend (Go)"),
		Debug:           getEnvAsBool("DEBUG", false),
		Port:            getEnvAsInt("PORT", 8000),
		BLENamePrefix:   getEnv("BLE_NAME_PREFIX", "ESP-Setup-"),
		MockBLE:         getEnvAsBool("MOCK_BLE", true),
		WiFiServiceUUID: getEnv("WIFI_SERVICE_UUID", "12345678-1234-5678-1234-56789abcdef0"),
		WiFiConfigChar:  getEnv("WIFI_CONFIG_CHAR_UUID", "12345678-1234-5678-1234-56789abcdef1"),
		WiFiStatusChar:  getEnv("WIFI_STATUS_CHAR_UUID", "12345678-1234-5678-1234-56789abcdef2"),

		MQTTEnabled:            getEnvAsBool("MQTT_ENABLED", true),
		MQTTBrokerHost:        getEnv("MQTT_BROKER_HOST", "localhost"),
		MQTTBrokerPort:        getEnvAsInt("MQTT_BROKER_PORT", 1883),
		MQTTUsername:          getEnv("MQTT_USERNAME", ""),
		MQTTPassword:          getEnv("MQTT_PASSWORD", ""),
		// Defaults align with firmware's `wifi_mqtt_topic_device(..., "cmd"/"data")`:
		// publish -> stopwatch/<device_id>/cmd
		// subscribe -> stopwatch/+/data
		MQTTPublishTopic:      getEnv("MQTT_PUBLISH_TOPIC", "stopwatch/{device_id}/cmd"),
		MQTTSubscribeTopic:    getEnv("MQTT_SUBSCRIBE_TOPIC", "stopwatch/+/data"),
		MQTTClientID:          getEnv("MQTT_CLIENT_ID", ""),
		MQTTConnectTimeoutSec: getEnvAsInt("MQTT_CONNECT_TIMEOUT_SEC", 5),
	}
	return cfg, nil
}

func configForDisplay(cfg *Config) map[string]any {
	return map[string]any{
		"app_name":             cfg.AppName,
		"debug":                cfg.Debug,
		"port":                 cfg.Port,
		"ble_name_prefix":      cfg.BLENamePrefix,
		"mock_ble":             cfg.MockBLE,
		"wifi_service_uuid":    cfg.WiFiServiceUUID,
		"wifi_config_char_uuid": cfg.WiFiConfigChar,
		"wifi_status_char_uuid": cfg.WiFiStatusChar,

		"mqtt_enabled":      cfg.MQTTEnabled,
		"mqtt_broker_host": cfg.MQTTBrokerHost,
		"mqtt_broker_port": cfg.MQTTBrokerPort,
		"mqtt_publish_topic":  cfg.MQTTPublishTopic,
		"mqtt_subscribe_topic": cfg.MQTTSubscribeTopic,
		"mqtt_username":    cfg.MQTTUsername,
		// Intentionally omit password (secrets).
		"_go_backend":          true,
	}
}

func connectMQTT(logger *log.Logger, cfg *Config) mqtt.Client {
	if !cfg.MQTTEnabled {
		logger.Printf("MQTT disabled (MQTT_ENABLED=false)")
		return nil
	}

	brokerURL := fmt.Sprintf("tcp://%s:%d", cfg.MQTTBrokerHost, cfg.MQTTBrokerPort)

	clientID := strings.TrimSpace(cfg.MQTTClientID)
	if clientID == "" {
		clientID = fmt.Sprintf("stopwatch-go-%d", time.Now().UnixNano())
	}

	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectTimeout(time.Duration(cfg.MQTTConnectTimeoutSec) * time.Second)

	if strings.TrimSpace(cfg.MQTTUsername) != "" {
		opts = opts.SetUsername(cfg.MQTTUsername).SetPassword(cfg.MQTTPassword)
	}

	opts.OnConnect = func(client mqtt.Client) {
		logger.Printf("Connected to MQTT broker %s", brokerURL)
		subscribeTopic := strings.TrimSpace(cfg.MQTTSubscribeTopic)
		if subscribeTopic == "" {
			logger.Printf("MQTT subscribe topic is empty; skipping subscribe")
			return
		}

		// Subscribe on every (re)connect to ensure data stream continues after reconnect.
		subToken := client.Subscribe(subscribeTopic, 0, func(_ mqtt.Client, msg mqtt.Message) {
			handleStopwatchMQTTMessage(logger, msg.Topic(), string(msg.Payload()))
		})
		if !subToken.WaitTimeout(5 * time.Second) {
			logger.Printf("MQTT subscribe timed out for topic %s", subscribeTopic)
			return
		}
		if err := subToken.Error(); err != nil {
			logger.Printf("MQTT subscribe error for topic %s: %v", subscribeTopic, err)
			return
		}
		logger.Printf("Subscribed to MQTT topic %s", subscribeTopic)
	}
	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		logger.Printf("MQTT connection lost: %v", err)
	}

	c := mqtt.NewClient(opts)

	logger.Printf("Connecting to MQTT broker %s", brokerURL)
	token := c.Connect()

	// Wait up to connect timeout for initial connection result.
	timeout := time.Duration(cfg.MQTTConnectTimeoutSec) * time.Second
	if !token.WaitTimeout(timeout) {
		logger.Printf("MQTT connect timed out after %s", timeout)
		return c
	}
	if err := token.Error(); err != nil {
		logger.Printf("MQTT connect error: %v", err)
		return c
	}

	return c
}

func handleStopwatchMQTTMessage(logger *log.Logger, topic, payload string) {
	deviceID, ok := parseDeviceIDFromStopwatchTopic(topic)
	if !ok {
		// Keep this warning because a wrong topic pattern means bad routing.
		logger.Printf("Ignoring MQTT message with unexpected topic: %s", topic)
		return
	}

	payload = strings.TrimSpace(payload)
	value := payload
	status := ""

	// New device format: {"data":"11:22:33.55","status":"start"}
	var parsed mqttStopwatchPayload
	if err := json.Unmarshal([]byte(payload), &parsed); err == nil && strings.TrimSpace(parsed.Data) != "" {
		value = strings.TrimSpace(parsed.Data)
		status = strings.ToLower(strings.TrimSpace(parsed.Status))
	}

	if !stopwatchDataRe.MatchString(value) {
		// Validate format strictly: 00:00:00.00
		logger.Printf("Ignoring MQTT payload with invalid stopwatch format (topic=%s payload=%q)", topic, payload)
		return
	}
	if status != "" && status != "start" && status != "stop" && status != "reset" {
		logger.Printf("Ignoring MQTT payload with invalid status (topic=%s status=%q)", topic, status)
		return
	}

	// Print real-time stopwatch data to terminal.
	if status == "" {
		logger.Printf("MQTT realtime device=%s data=%s", deviceID, value)
	} else {
		logger.Printf("MQTT realtime device=%s data=%s status=%s", deviceID, value, status)
	}

	ev := StopwatchRealtimeEvent{
		DeviceID:   deviceID,
		Data:       value,
		Status:     status,
		Topic:      topic,
		ReceivedAt: time.Now().UTC(),
	}
	publishRealtimeEvent(ev)
}

func parseDeviceIDFromStopwatchTopic(topic string) (string, bool) {
	// Expected pattern: stopwatch/{device_id}/data
	parts := strings.Split(topic, "/")
	if len(parts) != 3 {
		return "", false
	}
	if parts[0] != "stopwatch" || parts[2] != "data" {
		return "", false
	}
	deviceID := strings.TrimSpace(parts[1])
	if deviceID == "" {
		return "", false
	}
	return deviceID, true
}

func registerRealtimeClient(ch chan StopwatchRealtimeEvent) {
	realtimeHub.mu.Lock()
	realtimeHub.clients[ch] = struct{}{}
	realtimeHub.mu.Unlock()
}

func unregisterRealtimeClient(ch chan StopwatchRealtimeEvent) {
	realtimeHub.mu.Lock()
	delete(realtimeHub.clients, ch)
	close(ch)
	realtimeHub.mu.Unlock()
}

func publishRealtimeEvent(ev StopwatchRealtimeEvent) {
	realtimeHub.mu.Lock()
	realtimeHub.latest[ev.DeviceID] = ev

	for ch := range realtimeHub.clients {
		select {
		case ch <- ev:
		default:
			// Client is slow; drop this event for that client to avoid blocking.
		}
	}
	realtimeHub.mu.Unlock()
}

func latestRealtimeSnapshot() []StopwatchRealtimeEvent {
	realtimeHub.mu.RLock()
	out := make([]StopwatchRealtimeEvent, 0, len(realtimeHub.latest))
	for _, ev := range realtimeHub.latest {
		out = append(out, ev)
	}
	realtimeHub.mu.RUnlock()
	return out
}

func buildPublishTopic(template, deviceID string) string {
	topic := strings.TrimSpace(template)
	if topic == "" {
		return fmt.Sprintf("stopwatch/%s/cmd", deviceID)
	}
	if strings.Contains(topic, "{device_id}") {
		return strings.ReplaceAll(topic, "{device_id}", deviceID)
	}
	return topic
}

func publishMQTTMessage(topic, payload string) error {
	if mqttClient == nil {
		return fmt.Errorf("mqtt client is not initialized")
	}
	if !mqttClient.IsConnected() {
		return fmt.Errorf("mqtt client is not connected")
	}

	token := mqttClient.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(3 * time.Second) {
		return fmt.Errorf("mqtt publish timeout")
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt publish error: %w", err)
	}
	return nil
}


func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("failed to write JSON response: %v", err)
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func getEnv(key, defaultVal string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return defaultVal
}

func getEnvAsBool(key string, defaultVal bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func getEnvAsInt(key string, defaultVal int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return i
}

