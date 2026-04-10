package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		mu               sync.RWMutex
		latest           map[string]StopwatchRealtimeEvent
		latestHealth     map[string]DeviceHeartbeatEvent
		latestTiming     map[string]DeviceTimeEvent
		latestServerTick map[string]ServerTickEvent
		clients          map[chan streamEvent]struct{}
	}{
		latest:           make(map[string]StopwatchRealtimeEvent),
		latestHealth:     make(map[string]DeviceHeartbeatEvent),
		latestTiming:     make(map[string]DeviceTimeEvent),
		latestServerTick: make(map[string]ServerTickEvent),
		clients:          make(map[chan streamEvent]struct{}),
	}

	// Per-device server-side stopwatch (started when UI sends "start" after MQTT publish).
	serverStopMu sync.Mutex
	serverRuns   = map[string]*serverRun{}
	// Elapsed ms to resume from after stop (not cleared on start; cleared on reset or overwritten on next stop).
	deviceLastElapsedMs = map[string]int64{}

	// Serialize config publish + subscribe handshake (same topic, QoS 1).
	configAckMu sync.Mutex

	// Per-device admin fields used to answer MQTT stopwatch/{id}/lcd/request on stopwatch/{id}/lcd/response.
	requestProfileMu       sync.RWMutex
	requestProfileByDevice = map[string]RequestResponseProfile{}
)

// RequestResponseProfile is stored per device_id and sent when the device publishes on .../lcd/request.
type RequestResponseProfile struct {
	AthleteName string `json:"athlete_name"`
	EventLabel  string `json:"event_label"`
	Eid         int    `json:"eid"`
	Mid         int    `json:"mid"`
	Heart       int    `json:"heart"`
}

type serverRun struct {
	cancel   context.CancelFunc
	startAt  time.Time
	offsetMs int64 // carried from last stop / device time; added to wall elapsed each tick
}

// streamEvent is sent to SSE clients (stopwatch ticks or device heartbeat).
type streamEvent struct {
	Kind         string
	Stopwatch    StopwatchRealtimeEvent
	Heartbeat    DeviceHeartbeatEvent
	Timing       DeviceTimeEvent
	ServerTick   ServerTickEvent
	DeviceRename DeviceRenameEvent
}

// DeviceRenameEvent notifies the UI to drop a superseded device_id after server-side migration.
type DeviceRenameEvent struct {
	PreviousDeviceID string `json:"previous_device_id"`
	DeviceID         string `json:"device_id"`
}

// ServerTickEvent is emitted while the server-side stopwatch is running for a device.
type ServerTickEvent struct {
	DeviceID   string    `json:"device_id"`
	Display    string    `json:"display"`
	ElapsedMs  int64     `json:"elapsed_ms"`
	ReceivedAt time.Time `json:"received_at"`
}

// DeviceTimeEvent is emitted when MQTT receives stopwatch/{device_id}/time (device-reported result).
type DeviceTimeEvent struct {
	DeviceID   string         `json:"device_id"`
	Topic      string         `json:"topic"`
	Payload    map[string]any `json:"payload,omitempty"`
	TimeMs     int64          `json:"time_ms,omitempty"`
	Display    string         `json:"display"`
	ID         string         `json:"id,omitempty"`
	Type       int            `json:"type,omitempty"`
	Mid        int            `json:"mid,omitempty"`
	Eid        int            `json:"eid,omitempty"`
	Heart      int            `json:"heart,omitempty"`
	ReceivedAt time.Time      `json:"received_at"`
}

func ensureBLEAdapterEnabled() error {
	bleEnableOnce.Do(func() {
		bleEnableErr = bleAdapter.Enable()
	})
	return bleEnableErr
}

// Config holds application configuration (similar to the Python Settings model).
type Config struct {
	AppName         string
	Debug           bool
	Port            int
	BLENamePrefix   string
	MockBLE         bool
	WiFiServiceUUID string
	WiFiConfigChar  string
	WiFiStatusChar  string

	// MQTT broker configuration (mirrors Python backend defaults where possible).
	MQTTEnabled    bool
	MQTTBrokerHost string
	MQTTBrokerPort int
	MQTTUsername   string
	MQTTPassword   string
	// MQTT publish/subscribe topics.
	// These are not used yet by HTTP endpoints, but they are exposed in /api/config
	// so the frontend can confirm they are loaded.
	//
	// MQTTPublishTopic is used for commands and for config (lane/watch) — default stopwatch/{device_id}/cmd.
	MQTTPublishTopic   string
	MQTTSubscribeTopic string
	// MQTTSubscribeHealthTopic subscribes to device heartbeat (default: stopwatch/+/health).
	MQTTSubscribeHealthTopic string
	// MQTTSubscribeTimeTopic subscribes to device timing result (default: stopwatch/+/time).
	MQTTSubscribeTimeTopic string
	// MQTTSubscribeRequestTopic subscribes to device LCD request (default: stopwatch/+/lcd/request).
	MQTTSubscribeRequestTopic string
	// MQTTPublishResponseTopic publishes the server's answer (default: stopwatch/{device_id}/lcd/response).
	MQTTPublishResponseTopic string
	MQTTClientID             string
	MQTTConnectTimeoutSec    int
	// MQTTConfigAckTimeoutSec is how long to wait for {"id":"..."} on the cmd topic after publishing config.
	MQTTConfigAckTimeoutSec int
	// MQTTCmdPayloadPlain, if true, publishes only the command word (start|stop|reset) for firmware that does not parse JSON.
	MQTTCmdPayloadPlain bool
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
	TimeMs     int64     `json:"time_ms,omitempty"` // set when JSON payload includes "time" (milliseconds)
	Status     string    `json:"status,omitempty"`
	Topic      string    `json:"topic"`
	ReceivedAt time.Time `json:"received_at"`
}

// DeviceHeartbeatEvent is emitted when MQTT receives stopwatch/{device_id}/health.
type DeviceHeartbeatEvent struct {
	DeviceID   string         `json:"device_id"`
	ID         string         `json:"id,omitempty"`
	Battery    int            `json:"bat,omitempty"`
	RSSI       int            `json:"rssi,omitempty"`
	Status     string         `json:"status,omitempty"`
	Topic      string         `json:"topic"`
	Payload    map[string]any `json:"payload,omitempty"`
	ReceivedAt time.Time      `json:"received_at"`
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
		ch := make(chan streamEvent, 256)
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
		for _, ev := range latestHeartbeatSnapshot() {
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", b)
			flusher.Flush()
		}
		for _, ev := range latestServerTickSnapshot() {
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: server_tick\ndata: %s\n\n", b)
			flusher.Flush()
		}
		for _, ev := range latestTimingSnapshot() {
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: timing\ndata: %s\n\n", b)
			flusher.Flush()
		}

		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-ch:
				switch msg.Kind {
				case "heartbeat":
					b, err := json.Marshal(msg.Heartbeat)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", b)
				case "stopwatch":
					b, err := json.Marshal(msg.Stopwatch)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: stopwatch\ndata: %s\n\n", b)
				case "server_tick":
					b, err := json.Marshal(msg.ServerTick)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: server_tick\ndata: %s\n\n", b)
				case "timing":
					b, err := json.Marshal(msg.Timing)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: timing\ndata: %s\n\n", b)
				case "device_rename":
					b, err := json.Marshal(msg.DeviceRename)
					if err != nil {
						continue
					}
					_, _ = fmt.Fprintf(w, "event: device_rename\ndata: %s\n\n", b)
				default:
					continue
				}
				flusher.Flush()
			}
		}
	})

	// POST /api/mqtt/devices/{device_id}/{command}
	// Publishes command payload to stopwatch/{device_id}/cmd (template from MQTT_PUBLISH_TOPIC).
	// Supported commands: start, stop, reset.
	// POST /api/mqtt/devices/{device_id}/config publishes {"id","lane","watch"} to stopwatch/{device_id}/cmd (same as MQTT_PUBLISH_TOPIC).
	mux.HandleFunc("/api/mqtt/devices/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/mqtt/devices/") {
			http.NotFound(w, r)
			return
		}
		trim := strings.TrimPrefix(r.URL.Path, "/api/mqtt/devices/")
		trim = strings.TrimSuffix(trim, "/")
		if deviceID, ok := strings.CutSuffix(trim, "/request-response"); ok {
			deviceID = strings.TrimSpace(deviceID)
			if deviceID == "" {
				http.Error(w, "missing device_id in path", http.StatusBadRequest)
				return
			}
			switch r.Method {
			case http.MethodGet:
				getRequestResponseProfile(w, deviceID)
				return
			case http.MethodPut:
				putRequestResponseProfile(w, r, deviceID)
				return
			default:
				methodNotAllowed(w)
				return
			}
		}
		if !(strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/stop") || strings.HasSuffix(r.URL.Path, "/reset") || strings.HasSuffix(r.URL.Path, "/config")) {
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

		if command == "config" {
			bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 8192))
			if err != nil {
				http.Error(w, "failed to read body", http.StatusBadRequest)
				return
			}
			if len(strings.TrimSpace(string(bodyBytes))) == 0 {
				http.Error(w, "JSON body with lane and watch is required", http.StatusBadRequest)
				return
			}
			var cfgBody struct {
				Lane  *int `json:"lane"`
				Watch *int `json:"watch"`
			}
			if err := json.Unmarshal(bodyBytes, &cfgBody); err != nil {
				http.Error(w, "body must be JSON with lane and watch", http.StatusBadRequest)
				return
			}
			if cfgBody.Lane == nil || cfgBody.Watch == nil {
				http.Error(w, "lane and watch are required", http.StatusBadRequest)
				return
			}
			out := map[string]any{
				"id":    deviceID,
				"lane":  *cfgBody.Lane,
				"watch": *cfgBody.Watch,
			}
			payloadBytes, err := json.Marshal(out)
			if err != nil {
				http.Error(w, "failed to build config payload", http.StatusInternalServerError)
				return
			}
			payload := string(payloadBytes)
			topic := buildMQTTTopic(cfg.MQTTPublishTopic, deviceID, "stopwatch/{device_id}/cmd")
			if topic == "" {
				http.Error(w, "invalid device_id for mqtt topic", http.StatusBadRequest)
				return
			}
			previousID := deviceID
			ackTimeout := time.Duration(cfg.MQTTConfigAckTimeoutSec) * time.Second
			if ackTimeout <= 0 {
				ackTimeout = 15 * time.Second
			}
			ackID, ackOK, err := publishConfigAndWaitForAck(topic, payload, ackTimeout)
			if err != nil {
				log.Printf("mqtt config publish/handshake failed (topic=%s): %v", topic, err)
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			resolvedID := previousID
			if ackOK && ackID != "" {
				resolvedID = ackID
				if ackID != previousID {
					migrateDeviceID(previousID, ackID)
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"success":            true,
				"device_id":          resolvedID,
				"previous_device_id": previousID,
				"config_ack":         ackOK && ackID != "",
				"topic":              topic,
				"message":            payload,
			})
			return
		}

		if command != "start" && command != "stop" && command != "reset" {
			http.Error(w, "unsupported command", http.StatusBadRequest)
			return
		}

		var payload string
		if cfg.MQTTCmdPayloadPlain {
			payload = command
		} else {
			cmdPayload := map[string]any{
				"id":  deviceID,
				"cmd": command,
			}
			payloadBytes, err := json.Marshal(cmdPayload)
			if err != nil {
				http.Error(w, "failed to build command payload", http.StatusInternalServerError)
				return
			}
			payload = string(payloadBytes)
		}
		topic := buildMQTTTopic(cfg.MQTTPublishTopic, deviceID, "stopwatch/{device_id}/cmd")
		if topic == "" {
			http.Error(w, "invalid device_id for mqtt topic", http.StatusBadRequest)
			return
		}
		if err := publishMQTTMessage(topic, payload); err != nil {
			log.Printf("mqtt publish failed (topic=%s): %v", topic, err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("mqtt cmd published topic=%s payload=%s", topic, payload)

		switch command {
		case "start":
			startServerStopwatch(deviceID)
		case "stop":
			stopServerStopwatch(deviceID)
		case "reset":
			stopServerStopwatch(deviceID)
			clearDeviceLastElapsed(deviceID)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"success":   true,
			"device_id": deviceID,
			"topic":     topic,
			"message":   payload,
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

		MQTTEnabled:    getEnvAsBool("MQTT_ENABLED", true),
		MQTTBrokerHost: getEnv("MQTT_BROKER_HOST", "localhost"),
		MQTTBrokerPort: getEnvAsInt("MQTT_BROKER_PORT", 1883),
		MQTTUsername:   getEnv("MQTT_USERNAME", ""),
		MQTTPassword:   getEnv("MQTT_PASSWORD", ""),
		// Defaults align with firmware's `wifi_mqtt_topic_device(..., "cmd"/"data")`:
		// publish -> stopwatch/<device_id>/cmd
		// subscribe -> stopwatch/+/data
		MQTTPublishTopic:          getEnv("MQTT_PUBLISH_TOPIC", "stopwatch/{device_id}/cmd"),
		MQTTSubscribeTopic:        getEnv("MQTT_SUBSCRIBE_TOPIC", "stopwatch/+/data"),
		MQTTSubscribeHealthTopic:  getEnv("MQTT_SUBSCRIBE_HEALTH_TOPIC", "stopwatch/+/health"),
		MQTTSubscribeTimeTopic:    getEnv("MQTT_SUBSCRIBE_TIME_TOPIC", "stopwatch/+/time"),
		MQTTSubscribeRequestTopic: getEnv("MQTT_SUBSCRIBE_REQUEST_TOPIC", "stopwatch/+/lcd/request"),
		MQTTPublishResponseTopic:  getEnv("MQTT_PUBLISH_RESPONSE_TOPIC", "stopwatch/{device_id}/lcd/response"),
		MQTTClientID:              getEnv("MQTT_CLIENT_ID", ""),
		MQTTConnectTimeoutSec:     getEnvAsInt("MQTT_CONNECT_TIMEOUT_SEC", 5),
		MQTTConfigAckTimeoutSec:   getEnvAsInt("MQTT_CONFIG_ACK_TIMEOUT_SEC", 15),
		MQTTCmdPayloadPlain:       getEnvAsBool("MQTT_CMD_PAYLOAD_PLAIN", false),
	}
	return cfg, nil
}

func configForDisplay(cfg *Config) map[string]any {
	return map[string]any{
		"app_name":              cfg.AppName,
		"debug":                 cfg.Debug,
		"port":                  cfg.Port,
		"ble_name_prefix":       cfg.BLENamePrefix,
		"mock_ble":              cfg.MockBLE,
		"wifi_service_uuid":     cfg.WiFiServiceUUID,
		"wifi_config_char_uuid": cfg.WiFiConfigChar,
		"wifi_status_char_uuid": cfg.WiFiStatusChar,

		"mqtt_enabled":                 cfg.MQTTEnabled,
		"mqtt_broker_host":             cfg.MQTTBrokerHost,
		"mqtt_broker_port":             cfg.MQTTBrokerPort,
		"mqtt_publish_topic":           cfg.MQTTPublishTopic,
		"mqtt_subscribe_topic":         cfg.MQTTSubscribeTopic,
		"mqtt_subscribe_health_topic":  cfg.MQTTSubscribeHealthTopic,
		"mqtt_subscribe_time_topic":    cfg.MQTTSubscribeTimeTopic,
		"mqtt_subscribe_request_topic": cfg.MQTTSubscribeRequestTopic,
		"mqtt_publish_response_topic":  cfg.MQTTPublishResponseTopic,
		"mqtt_username":                cfg.MQTTUsername,
		"mqtt_config_ack_timeout_sec":  cfg.MQTTConfigAckTimeoutSec,
		"mqtt_cmd_payload_plain":       cfg.MQTTCmdPayloadPlain,
		// Intentionally omit password (secrets).
		"_go_backend": true,
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

		onMessage := func(_ mqtt.Client, msg mqtt.Message) {
			topic := msg.Topic()
			payload := string(msg.Payload())
			switch {
			case strings.HasSuffix(topic, "/data"):
				handleStopwatchMQTTMessage(logger, topic, payload)
			case strings.HasSuffix(topic, "/health"):
				handleHeartbeatMQTTMessage(logger, topic, payload)
			case strings.HasSuffix(topic, "/time"):
				handleTimeMQTTMessage(logger, topic, payload)
			case strings.HasSuffix(topic, "/lcd/request"):
				handleRequestMQTTMessage(logger, cfg, topic, payload)
			default:
				logger.Printf("Ignoring MQTT message on unexpected topic: %s", topic)
			}
		}

		for _, subscribeTopic := range mqttSubscribeTopics(cfg) {
			subscribeTopic = strings.TrimSpace(subscribeTopic)
			if subscribeTopic == "" {
				continue
			}
			subToken := client.Subscribe(subscribeTopic, 0, onMessage)
			if !subToken.WaitTimeout(5 * time.Second) {
				logger.Printf("MQTT subscribe timed out for topic %s", subscribeTopic)
				continue
			}
			if err := subToken.Error(); err != nil {
				logger.Printf("MQTT subscribe error for topic %s: %v", subscribeTopic, err)
				continue
			}
			logger.Printf("Subscribed to MQTT topic %s", subscribeTopic)
		}
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

func mqttSubscribeTopics(cfg *Config) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, t := range []string{cfg.MQTTSubscribeTopic, cfg.MQTTSubscribeHealthTopic, cfg.MQTTSubscribeTimeTopic, cfg.MQTTSubscribeRequestTopic} {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func handleStopwatchMQTTMessage(logger *log.Logger, topic, payload string) {
	deviceID, ok := parseDeviceIDFromTopic(topic, "data")
	if !ok {
		// Keep this warning because a wrong topic pattern means bad routing.
		logger.Printf("Ignoring MQTT message with unexpected topic: %s", topic)
		return
	}
	if isServerRunning(deviceID) {
		// While the server-side stopwatch is running, live /data updates are suppressed so the UI follows server ticks.
		return
	}

	payload = strings.TrimSpace(payload)
	var value string
	status := ""
	var timeMs int64
	hasTimeMs := false

	// JSON: prefer numeric "time" (milliseconds) for the displayed stopwatch; else use string "data" (HH:MM:SS.hh).
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err == nil {
		if v, ok := raw["time"]; ok {
			if ms, ok := int64FromJSONAny(v); ok {
				timeMs = ms
				hasTimeMs = true
				value = formatDurationToWatch(ms)
			}
		}
		if s, ok := raw["status"].(string); ok {
			status = strings.ToLower(strings.TrimSpace(s))
		}
		if !hasTimeMs {
			if d, ok := raw["data"].(string); ok {
				value = strings.TrimSpace(d)
			}
		}
	}
	if value == "" {
		value = payload
	}

	if !hasTimeMs && !stopwatchDataRe.MatchString(value) {
		// Without "time" (ms), require legacy HH:MM:SS.hh in data or raw payload.
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
	if hasTimeMs {
		ev.TimeMs = timeMs
	}
	publishRealtimeEvent(ev)
}

func handleHeartbeatMQTTMessage(logger *log.Logger, topic, payload string) {
	deviceID, ok := parseDeviceIDFromTopic(topic, "health")
	if !ok {
		logger.Printf("Ignoring MQTT heartbeat with unexpected topic: %s", topic)
		return
	}

	payload = strings.TrimSpace(payload)
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		logger.Printf("Ignoring invalid heartbeat JSON (topic=%s): %v", topic, err)
		return
	}

	ev := DeviceHeartbeatEvent{
		DeviceID:   deviceID,
		Topic:      topic,
		Payload:    raw,
		ReceivedAt: time.Now().UTC(),
	}

	if v, ok := raw["id"].(string); ok {
		ev.ID = strings.TrimSpace(v)
	}
	if v, ok := raw["bat"]; ok {
		switch n := v.(type) {
		case float64:
			ev.Battery = int(n)
		case int:
			ev.Battery = n
		case int64:
			ev.Battery = int(n)
		}
	}
	if v, ok := raw["rssi"]; ok {
		switch n := v.(type) {
		case float64:
			ev.RSSI = int(n)
		case int:
			ev.RSSI = n
		case int64:
			ev.RSSI = int(n)
		}
	}
	if v, ok := raw["status"].(string); ok {
		ev.Status = strings.TrimSpace(v)
	}

	logger.Printf("MQTT heartbeat device=%s id=%s bat=%d rssi=%d status=%s", deviceID, ev.ID, ev.Battery, ev.RSSI, ev.Status)
	publishHeartbeatEvent(ev)
}

func handleTimeMQTTMessage(logger *log.Logger, topic, payload string) {
	deviceID, ok := parseDeviceIDFromTopic(topic, "time")
	if !ok {
		logger.Printf("Ignoring MQTT time with unexpected topic: %s", topic)
		return
	}

	stopServerStopwatch(deviceID)

	payload = strings.TrimSpace(payload)
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		logger.Printf("Ignoring invalid time JSON (topic=%s): %v", topic, err)
		return
	}

	var timeMs int64
	if v, ok := raw["time"]; ok {
		if n, ok := int64FromJSONAny(v); ok {
			timeMs = n
		}
	}

	// Next Start resumes from device-reported time (ms).
	setDeviceLastElapsed(deviceID, timeMs)

	ev := DeviceTimeEvent{
		DeviceID:   deviceID,
		Topic:      topic,
		Payload:    raw,
		TimeMs:     timeMs,
		Display:    formatDurationToWatch(timeMs),
		ReceivedAt: time.Now().UTC(),
	}
	if v, ok := raw["id"].(string); ok {
		ev.ID = strings.TrimSpace(v)
	}
	if v, ok := raw["type"]; ok {
		if n, ok := intFromJSONAny(v); ok {
			ev.Type = n
		}
	}
	if v, ok := raw["mid"]; ok {
		if n, ok := intFromJSONAny(v); ok {
			ev.Mid = n
		}
	}
	if v, ok := raw["eid"]; ok {
		if n, ok := intFromJSONAny(v); ok {
			ev.Eid = n
		}
	}
	if v, ok := raw["heart"]; ok {
		if n, ok := intFromJSONAny(v); ok {
			ev.Heart = n
		}
	}

	logger.Printf("MQTT time device=%s time_ms=%d display=%s", deviceID, timeMs, ev.Display)
	publishTimingEvent(ev)
}

func handleRequestMQTTMessage(logger *log.Logger, cfg *Config, topic, payload string) {
	deviceID, ok := parseDeviceIDFromLCDTopic(topic, "request")
	if !ok {
		logger.Printf("Ignoring MQTT request with unexpected topic: %s", topic)
		return
	}

	payload = strings.TrimSpace(payload)
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		logger.Printf("Ignoring invalid request JSON (topic=%s): %v", topic, err)
		return
	}
	lane := 0
	if v, ok := raw["lane"]; ok {
		if n, ok := intFromJSONAny(v); ok {
			lane = n
		}
	}

	requestProfileMu.RLock()
	prof := requestProfileByDevice[deviceID]
	requestProfileMu.RUnlock()

	out := map[string]any{
		"lane":         lane,
		"athlete_name": prof.AthleteName,
		"event_label":  prof.EventLabel,
		"eid":          prof.Eid,
		"mid":          prof.Mid,
		"heart":        prof.Heart,
	}
	respBytes, err := json.Marshal(out)
	if err != nil {
		logger.Printf("MQTT response marshal failed: %v", err)
		return
	}
	respTopic := buildMQTTTopic(cfg.MQTTPublishResponseTopic, deviceID, "stopwatch/{device_id}/lcd/response")
	if respTopic == "" {
		logger.Printf("Missing device_id for MQTT response topic")
		return
	}
	if err := publishMQTTMessage(respTopic, string(respBytes)); err != nil {
		logger.Printf("mqtt response publish failed (topic=%s): %v", respTopic, err)
		return
	}
	logger.Printf("MQTT request answered device=%s lane=%d -> %s", deviceID, lane, respTopic)
}

func formatDurationToWatch(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	frac := (ms % 1000) / 10
	if frac > 99 {
		frac = 99
	}
	return fmt.Sprintf("%02d:%02d:%02d.%02d", h, m, s, frac)
}

func int64FromJSONAny(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func intFromJSONAny(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.Atoi(s)
		return i, err == nil
	default:
		return 0, false
	}
}

func isServerRunning(deviceID string) bool {
	serverStopMu.Lock()
	defer serverStopMu.Unlock()
	_, ok := serverRuns[deviceID]
	return ok
}

func startServerStopwatch(deviceID string) {
	serverStopMu.Lock()
	if r, ok := serverRuns[deviceID]; ok {
		elapsed := time.Since(r.startAt).Milliseconds() + r.offsetMs
		if elapsed < 0 {
			elapsed = 0
		}
		deviceLastElapsedMs[deviceID] = elapsed
		r.cancel()
		delete(serverRuns, deviceID)
	}
	offsetMs := deviceLastElapsedMs[deviceID]
	ctx, cancel := context.WithCancel(context.Background())
	startAt := time.Now()
	serverRuns[deviceID] = &serverRun{cancel: cancel, startAt: startAt, offsetMs: offsetMs}
	serverStopMu.Unlock()

	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ms := time.Since(startAt).Milliseconds() + offsetMs
				if ms < 0 {
					ms = 0
				}
				display := formatDurationToWatch(ms)
				publishServerTick(deviceID, ms, display)
			}
		}
	}()
}

func stopServerStopwatch(deviceID string) {
	serverStopMu.Lock()
	defer serverStopMu.Unlock()
	if r, ok := serverRuns[deviceID]; ok {
		elapsed := time.Since(r.startAt).Milliseconds() + r.offsetMs
		if elapsed < 0 {
			elapsed = 0
		}
		deviceLastElapsedMs[deviceID] = elapsed
		r.cancel()
		delete(serverRuns, deviceID)
	}
}

func setDeviceLastElapsed(deviceID string, ms int64) {
	if ms < 0 {
		ms = 0
	}
	serverStopMu.Lock()
	deviceLastElapsedMs[deviceID] = ms
	serverStopMu.Unlock()
}

func clearDeviceLastElapsed(deviceID string) {
	serverStopMu.Lock()
	delete(deviceLastElapsedMs, deviceID)
	serverStopMu.Unlock()
}

// migrateDeviceID moves per-device server and hub state when the canonical id changes after config ACK.
func migrateDeviceID(oldID, newID string) {
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	stopServerStopwatch(oldID)

	serverStopMu.Lock()
	if ms, ok := deviceLastElapsedMs[oldID]; ok {
		deviceLastElapsedMs[newID] = ms
		delete(deviceLastElapsedMs, oldID)
	}
	serverStopMu.Unlock()

	realtimeHub.mu.Lock()
	if ev, ok := realtimeHub.latest[oldID]; ok {
		ev.DeviceID = newID
		realtimeHub.latest[newID] = ev
		delete(realtimeHub.latest, oldID)
	}
	if ev, ok := realtimeHub.latestHealth[oldID]; ok {
		ev.DeviceID = newID
		realtimeHub.latestHealth[newID] = ev
		delete(realtimeHub.latestHealth, oldID)
	}
	if ev, ok := realtimeHub.latestTiming[oldID]; ok {
		ev.DeviceID = newID
		realtimeHub.latestTiming[newID] = ev
		delete(realtimeHub.latestTiming, oldID)
	}
	if ev, ok := realtimeHub.latestServerTick[oldID]; ok {
		ev.DeviceID = newID
		realtimeHub.latestServerTick[newID] = ev
		delete(realtimeHub.latestServerTick, oldID)
	}
	realtimeHub.mu.Unlock()

	requestProfileMu.Lock()
	if p, ok := requestProfileByDevice[oldID]; ok {
		requestProfileByDevice[newID] = p
		delete(requestProfileByDevice, oldID)
	}
	requestProfileMu.Unlock()

	publishDeviceRenameEvent(oldID, newID)
	log.Printf("device id migrated after config ack: %s -> %s", oldID, newID)
}

func publishDeviceRenameEvent(previousID, newID string) {
	if previousID == "" || newID == "" || previousID == newID {
		return
	}
	ev := DeviceRenameEvent{PreviousDeviceID: previousID, DeviceID: newID}
	msg := streamEvent{Kind: "device_rename", DeviceRename: ev}
	realtimeHub.mu.Lock()
	for ch := range realtimeHub.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	realtimeHub.mu.Unlock()
}

func parseConfigAckID(payload, sentPayload string) (string, bool) {
	p := strings.TrimSpace(payload)
	if p == strings.TrimSpace(sentPayload) {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(p), &m); err != nil {
		return "", false
	}
	raw, ok := m["id"]
	if !ok {
		return "", false
	}
	var id string
	switch v := raw.(type) {
	case string:
		id = strings.TrimSpace(v)
	default:
		id = strings.TrimSpace(fmt.Sprint(v))
	}
	if id == "" {
		return "", false
	}
	return id, true
}

func drainStringChan(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// publishConfigAndWaitForAck subscribes at QoS 1 to the same cmd topic, publishes the config payload (QoS 1),
// then waits for a distinct JSON message containing a non-empty "id" (device ACK). ackOK is false on timeout.
func publishConfigAndWaitForAck(topic, sentPayload string, timeout time.Duration) (ackID string, ackOK bool, err error) {
	if mqttClient == nil {
		return "", false, fmt.Errorf("mqtt client is not initialized")
	}
	if !mqttClient.IsConnected() {
		return "", false, fmt.Errorf("mqtt client is not connected")
	}

	configAckMu.Lock()
	defer configAckMu.Unlock()

	msgCh := make(chan string, 2)
	handler := func(_ mqtt.Client, msg mqtt.Message) {
		p := string(msg.Payload())
		if id, ok := parseConfigAckID(p, sentPayload); ok {
			select {
			case msgCh <- id:
			default:
			}
		}
	}

	subTok := mqttClient.Subscribe(topic, 1, handler)
	if !subTok.WaitTimeout(5 * time.Second) {
		return "", false, fmt.Errorf("mqtt subscribe timeout for %s", topic)
	}
	if err := subTok.Error(); err != nil {
		return "", false, fmt.Errorf("mqtt subscribe error: %w", err)
	}
	defer func() {
		unsub := mqttClient.Unsubscribe(topic)
		if !unsub.WaitTimeout(3 * time.Second) {
			log.Printf("mqtt unsubscribe timeout for %s", topic)
			return
		}
		if err := unsub.Error(); err != nil {
			log.Printf("mqtt unsubscribe error for %s: %v", topic, err)
		}
	}()

	drainStringChan(msgCh)
	time.Sleep(50 * time.Millisecond)
	drainStringChan(msgCh)

	if err := publishMQTTMessage(topic, sentPayload); err != nil {
		return "", false, err
	}

	select {
	case ackID = <-msgCh:
		return ackID, true, nil
	case <-time.After(timeout):
		return "", false, nil
	}
}

func parseDeviceIDFromTopic(topic, suffix string) (string, bool) {
	// Expected pattern: stopwatch/{device_id}/{suffix}
	parts := strings.Split(topic, "/")
	if len(parts) != 3 {
		return "", false
	}
	if parts[0] != "stopwatch" || parts[2] != suffix {
		return "", false
	}
	deviceID := strings.TrimSpace(parts[1])
	if deviceID == "" {
		return "", false
	}
	return deviceID, true
}

// parseDeviceIDFromLCDTopic parses stopwatch/{device_id}/lcd/{suffix} (suffix is "request" or "response").
func parseDeviceIDFromLCDTopic(topic, suffix string) (string, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 {
		return "", false
	}
	if parts[0] != "stopwatch" || parts[2] != "lcd" || parts[3] != suffix {
		return "", false
	}
	deviceID := strings.TrimSpace(parts[1])
	if deviceID == "" {
		return "", false
	}
	return deviceID, true
}

func registerRealtimeClient(ch chan streamEvent) {
	realtimeHub.mu.Lock()
	realtimeHub.clients[ch] = struct{}{}
	realtimeHub.mu.Unlock()
}

func unregisterRealtimeClient(ch chan streamEvent) {
	realtimeHub.mu.Lock()
	delete(realtimeHub.clients, ch)
	close(ch)
	realtimeHub.mu.Unlock()
}

func publishRealtimeEvent(ev StopwatchRealtimeEvent) {
	if isServerRunning(ev.DeviceID) {
		return
	}
	realtimeHub.mu.Lock()
	realtimeHub.latest[ev.DeviceID] = ev
	msg := streamEvent{Kind: "stopwatch", Stopwatch: ev}

	for ch := range realtimeHub.clients {
		select {
		case ch <- msg:
		default:
			// Client is slow; drop this event for that client to avoid blocking.
		}
	}
	realtimeHub.mu.Unlock()
}

func publishHeartbeatEvent(ev DeviceHeartbeatEvent) {
	realtimeHub.mu.Lock()
	realtimeHub.latestHealth[ev.DeviceID] = ev
	msg := streamEvent{Kind: "heartbeat", Heartbeat: ev}

	for ch := range realtimeHub.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	realtimeHub.mu.Unlock()
}

func publishServerTick(deviceID string, elapsedMs int64, display string) {
	ev := ServerTickEvent{
		DeviceID:   deviceID,
		Display:    display,
		ElapsedMs:  elapsedMs,
		ReceivedAt: time.Now().UTC(),
	}
	realtimeHub.mu.Lock()
	realtimeHub.latestServerTick[deviceID] = ev
	msg := streamEvent{Kind: "server_tick", ServerTick: ev}
	for ch := range realtimeHub.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	realtimeHub.mu.Unlock()
}

func publishTimingEvent(ev DeviceTimeEvent) {
	realtimeHub.mu.Lock()
	realtimeHub.latestTiming[ev.DeviceID] = ev
	msg := streamEvent{Kind: "timing", Timing: ev}
	for ch := range realtimeHub.clients {
		select {
		case ch <- msg:
		default:
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

func latestHeartbeatSnapshot() []DeviceHeartbeatEvent {
	realtimeHub.mu.RLock()
	out := make([]DeviceHeartbeatEvent, 0, len(realtimeHub.latestHealth))
	for _, ev := range realtimeHub.latestHealth {
		out = append(out, ev)
	}
	realtimeHub.mu.RUnlock()
	return out
}

func latestServerTickSnapshot() []ServerTickEvent {
	realtimeHub.mu.RLock()
	out := make([]ServerTickEvent, 0, len(realtimeHub.latestServerTick))
	for _, ev := range realtimeHub.latestServerTick {
		out = append(out, ev)
	}
	realtimeHub.mu.RUnlock()
	return out
}

func latestTimingSnapshot() []DeviceTimeEvent {
	realtimeHub.mu.RLock()
	out := make([]DeviceTimeEvent, 0, len(realtimeHub.latestTiming))
	for _, ev := range realtimeHub.latestTiming {
		out = append(out, ev)
	}
	realtimeHub.mu.RUnlock()
	return out
}

func getRequestResponseProfile(w http.ResponseWriter, deviceID string) {
	requestProfileMu.RLock()
	prof, ok := requestProfileByDevice[deviceID]
	requestProfileMu.RUnlock()
	if !ok {
		prof = RequestResponseProfile{}
	}
	writeJSON(w, http.StatusOK, prof)
}

func putRequestResponseProfile(w http.ResponseWriter, r *http.Request, deviceID string) {
	var prof RequestResponseProfile
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&prof); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	requestProfileMu.Lock()
	requestProfileByDevice[deviceID] = prof
	requestProfileMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "device_id": deviceID})
}

// buildMQTTTopic resolves a template with {device_id}. If the template is empty or missing the
// placeholder, defaultTemplate (must contain "{device_id}") is used so publishes always target
// a per-device topic (misconfigured MQTT_PUBLISH_TOPIC previously produced a single shared topic).
func buildMQTTTopic(template, deviceID, defaultTemplate string) string {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return ""
	}
	topic := strings.TrimSpace(template)
	if topic == "" {
		topic = defaultTemplate
	}
	if strings.Contains(topic, "{device_id}") {
		return strings.ReplaceAll(topic, "{device_id}", deviceID)
	}
	out := strings.ReplaceAll(strings.TrimSpace(defaultTemplate), "{device_id}", deviceID)
	log.Printf("mqtt: topic template %q has no {device_id} placeholder; using %q", topic, out)
	return out
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
