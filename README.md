## Go backend for ESP32 BLE provisioning

This Go application mirrors the main functionality of the FastAPI backend in `app/backend`:

- **`GET /health`**: returns a simple health check (`{"status":"ok"}`).
- **`GET /api/config`**: returns the current configuration that the Go app is using.
- **`GET /api/devices`**: scans for ESP32 devices over BLE and returns a list of devices (in mock mode by default).
- **`POST /api/devices/{device_id}/wifi`**: sends WiFi credentials to a specific device over BLE and returns a result whose `message` is whatever the device reports (e.g. IP address).
- **`GET /api/mqtt/stream`**: opens an SSE stream with real-time stopwatch data for frontend clients.
- **`POST /api/mqtt/devices/{device_id}/{command}`**: publishes stopwatch commands (`start`, `stop`, `reset`) to MQTT.

The JSON shapes for `DeviceSummary`, `WifiConfig`, `WifiProvisionResult`, and `HealthStatus` are compatible with the FastAPI models.

## Frontend REST API reference

Base URL (default): `http://localhost:8000`

### 1) Health and app config

- **`GET /health`**
  - Purpose: backend health check.
  - Success response:
    - `200 OK`
    - `{"status":"ok"}`

- **`GET /api/config`**
  - Purpose: expose effective runtime configuration used by frontend.
  - Success response:
    - `200 OK`
    - JSON object with fields such as `app_name`, `port`, `mock_ble`, `mqtt_enabled`, `mqtt_publish_topic`, `mqtt_subscribe_topic`.

### 2) BLE device discovery and WiFi provisioning

- **`GET /api/devices`**
  - Purpose: scan BLE and return matching devices.
  - Success response:
    - `200 OK`
    - JSON array of devices:
      - `id` (string)
      - `name` (string, optional)
      - `rssi` (number, optional)

- **`POST /api/devices/{device_id}/wifi`**
  - Purpose: send WiFi credentials to selected BLE device.
  - Path param:
    - `device_id` from `/api/devices`.
  - Request body (`application/json`):
    - `ssid` (required)
    - `password` (optional; alias of `pass`)
    - `pass` (optional; alias of `password`)
  - Example body:
    - `{"ssid":"MyWifi","password":"secret"}`
  - Success response:
    - `200 OK`
    - `{"success":true|false,"message":"..."}`  
      (`message` contains device status/IP when available)
  - Error responses:
    - `400 Bad Request` for invalid JSON or missing `ssid`.
    - `405 Method Not Allowed` for unsupported methods.

### 3) Stopwatch control and real-time updates

- **`POST /api/mqtt/devices/{device_id}/{command}`**
  - Purpose: publish stopwatch command to device via MQTT.
  - Path params:
    - `device_id` (target stopwatch device id).
    - `command` must be one of: `start`, `stop`, `reset`.
  - Request body:
    - none.
  - Success response:
    - `200 OK`
    - `{"success":true,"device_id":"...","topic":"...","message":"start|stop|reset"}`
  - Error responses:
    - `400 Bad Request` for invalid path/command.
    - `503 Service Unavailable` if MQTT is disconnected/unavailable.

- **`GET /api/mqtt/stream`** (SSE endpoint used by frontend)
  - Purpose: stream real-time stopwatch updates.
  - Response headers:
    - `Content-Type: text/event-stream`
    - `Cache-Control: no-cache`
    - `Connection: keep-alive`
  - SSE events:
    - `ready` event with `data: ok` when stream opens.
    - `stopwatch` events where `data` is JSON:
      - `device_id` (string)
      - `data` (format `HH:MM:SS.hh`)
      - `status` (`start|stop|reset`, optional)
      - `topic` (MQTT topic string)
      - `received_at` (UTC timestamp)
  - Frontend note:
    - use `EventSource('/api/mqtt/stream')` and parse `stopwatch` event payload.

### Prerequisites

- Go 1.22 or newer.
- Optional: a `.env` file in this directory if you want to override defaults.

### Configuration

The Go app reads configuration from environment variables (and an optional local `.env` file) similar to the Python `Settings`:

- **`APP_NAME`**: application name (default: `ESP32 Backend (Go)`).
- **`DEBUG`**: enable debug mode (`true`/`false`, default: `false`).
- **`PORT`**: HTTP port to listen on (default: `8000`).
- **`BLE_NAME_PREFIX`**: BLE device name prefix to filter on (default: `ESP-Setup-`).
- **`MOCK_BLE`**: when `true`, use an in-memory mock BLE implementation (default: `true`).
- **`WIFI_SERVICE_UUID`**: WiFi provisioning service UUID.
- **`WIFI_CONFIG_CHAR_UUID`**: characteristic UUID used to send WiFi config.
- **`WIFI_STATUS_CHAR_UUID`**: characteristic UUID used for status/indication.
- **`MQTT_ENABLED`**: connect to the MQTT broker on server startup (`true`/`false`, default: `true`).
- **`MQTT_BROKER_HOST`**: MQTT broker host (default: `localhost`).
- **`MQTT_BROKER_PORT`**: MQTT broker port (default: `1883`).
- **`MQTT_PUBLISH_TOPIC`**: publish topic template (default: `stopwatch/{device_id}/cmd`). Supports `{device_id}` placeholder.
- **`MQTT_SUBSCRIBE_TOPIC`**: subscribe topic (default: `stopwatch/+/data`).
- **`MQTT_USERNAME`**: optional MQTT username (default: empty).
- **`MQTT_PASSWORD`**: optional MQTT password (default: empty).
- **`MQTT_CONNECT_TIMEOUT_SEC`**: how long to wait for the initial MQTT connection (default: `5`).

By default `MOCK_BLE=true`, which is similar to the Python backend’s default; this returns fake devices and simulates WiFi provisioning so you can develop the frontend without needing real hardware.

### Running the server

From this directory (`app/golang`):

- **Install dependencies (first time only)**:

  ```bash
  go mod tidy
  ```

- **Run the server**:

  ```bash
  go run ./...
  ```

The server will start on `http://localhost:8000` by default (or the port specified by `PORT` in `.env` or the environment).

#### Example HTTP requests

- **Health check**:

  ```bash
  curl http://localhost:8000/health
  ```

- **Show config**:

  ```bash
  curl http://localhost:8000/api/config
  ```

- **List devices**:

  ```bash
  curl http://localhost:8000/api/devices
  ```

- **Configure WiFi for a device (Windows PowerShell example)**:

  ```powershell
  curl -X POST "http://localhost:8000/api/devices/FAKE:01/wifi" `
    -H "Content-Type: application/json" `
    -d "{\"ssid\":\"MyWifi\",\"password\":\"secret\"}"
  ```

- **Configure WiFi for a device (Linux/macOS bash example)**:

  ```bash
  curl -X POST "http://localhost:8000/api/devices/FAKE:01/wifi" \
    -H "Content-Type: application/json" \
    -d '{"ssid":"MyWifi","password":"secret"}'
  ```

In real BLE mode, replace `FAKE:01` with the actual `id` returned by `/api/devices`. The `message` field of the JSON response will contain the device status or IP address reported by the ESP32 firmware.

### Real BLE implementation

The `main.go` file defines a `BLEService` interface and provides:

- `mockBLEService`: fully functional mock, matching the Python `MOCK_BLE` behavior.
- `realBLEService`: a placeholder where you can plug in a real BLE library (for example, `github.com/go-ble/ble`) to:
  - Discover devices with names starting with `BLE_NAME_PREFIX`.
  - Connect to the device identified by `device_id`.
  - Write WiFi credentials as a JSON payload (`{"ssid": "...", "pass": "..."}`) to the config characteristic (`WIFI_CONFIG_CHAR_UUID`).
  - Subscribe to indications on that characteristic and use the indication payload (e.g. IP address) as the `message` field in `WifiProvisionResult`.

Once you implement `realBLEService`, you can run the Go app with:

```bash
set MOCK_BLE=false
go run ./...
```

