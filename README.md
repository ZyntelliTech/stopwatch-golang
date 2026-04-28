## Go backend for ESP32 BLE provisioning and stopwatch

This Go application mirrors the main functionality of the FastAPI backend in `app/backend`, and adds MQTT-driven stopwatch monitoring plus a small web dashboard.

### Quick feature list

- **`GET /health`**: health check (`{"status":"ok"}`).
- **`GET /api/config`**: effective runtime configuration (including MQTT topic names).
- **`GET /api/devices`**: BLE scan for ESP32 devices (mock BLE by default).
- **`POST /api/devices/{device_id}/wifi`**: provision WiFi over BLE for one device.
- **`POST /api/devices/wifi/batch`**: provision WiFi for multiple devices in one request (same SSID/credentials).
- **`GET /api/mqtt/stream`**: Server-Sent Events (SSE) stream for the dashboard (stopwatch, heartbeat, server ticks, device timing).
- **`POST /api/mqtt/devices/{device_id}/{command}`**: publish `start`, `stop`, or `reset` to MQTT (`stopwatch/{device_id}/cmd` by default) and run or stop the server-side stopwatch as described below.

The JSON shapes for `DeviceSummary`, `WifiConfig`, `WifiProvisionResult`, and `HealthStatus` are compatible with the FastAPI models.

### Web dashboard

Static UI is served at `/` and `/index.html` from `web/index.html`. It connects to **`GET /api/mqtt/stream`** and shows one card per `device_id`: live stopwatch, MQTT start/stop/reset, optional manual device entry, heartbeat fields, and device-reported timing when present.

---

## Frontend REST API reference

Base URL (default): `http://localhost:8000`

### 1) Health and app config

- **`GET /health`** → `200 OK`, `{"status":"ok"}`.

- **`GET /api/config`** → `200 OK`, JSON including for example:
  - `app_name`, `port`, `mock_ble`
  - `mqtt_enabled`, `mqtt_broker_host`, `mqtt_broker_port`
  - `mqtt_publish_topic`, `mqtt_subscribe_topic`, `mqtt_subscribe_health_topic`, `mqtt_subscribe_time_topic`
  - `mqtt_username` (password is never returned)

### 2) BLE device discovery and WiFi provisioning

- **`GET /api/devices`**  
  - `200 OK`: JSON array of `{ "id", "name"?, "rssi"? }`.

- **`POST /api/devices/{device_id}/wifi`**  
  - Body: `{"ssid":"...","password":"..."}` (or `"pass"` instead of `password"`).  
  - `200 OK`: `{"success":bool,"message":"..."}` (e.g. IP or status from firmware).

- **`POST /api/devices/wifi/batch`**  
  - Body: `{"device_ids":["id1","id2"],"ssid":"...","password":"..."}`.  
  - `200 OK`: `total`, `success`, `failed`, `results[]` per device.

### 3) MQTT commands and server-side stopwatch

- **`POST /api/mqtt/devices/{device_id}/{command}`**  
  - `command`: `start` | `stop` | `reset`.  
  - Publishes the command string to the configured MQTT publish topic (default `stopwatch/{device_id}/cmd`).  
  - **Start**: after a successful publish, the server starts a per-device stopwatch that emits **`server_tick`** SSE events (about every 50 ms). The displayed time **resumes from the last stopped value** for that `device_id` (not from zero), unless you use **reset** (see below).  
  - **Stop**: stops the server stopwatch and **stores** the current elapsed time for the next **start**.  
  - **Reset**: stops the server stopwatch and **clears** the stored elapsed time so the **next** start begins at `00:00:00.00`.  
  - `503` if MQTT publish fails (e.g. broker disconnected).

### 4) SSE: `GET /api/mqtt/stream`

- Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`.
- **`event: ready`** — `data: ok` when the connection is open.
- **`event: stopwatch`** — JSON from topic `stopwatch/{device_id}/data`:
  - `device_id`, `topic`, `received_at`
  - `data`: clock string `HH:MM:SS.hh` when the payload uses legacy `data`, **or** derived from numeric/string **`time`** (milliseconds).
  - `time_ms`: present when the JSON payload includes **`time`** (number or numeric string, in milliseconds).
  - `status`: optional (`start` / `stop` / `reset`).
  - While the **server** stopwatch is running for a device, **`/data` updates for that device are not forwarded** so the UI follows **`server_tick`** only.
- **`event: heartbeat`** — JSON from `stopwatch/{device_id}/health` (battery, RSSI, status, full payload map, etc.).
- **`event: server_tick`** — JSON while the server stopwatch is running:
  - `device_id`, `display`, `elapsed_ms`, `received_at`.
- **`event: timing`** — JSON from `stopwatch/{device_id}/time` (device-reported result after stop/reset), e.g.:
  - `time_ms`, `display`, `id`, `type`, `mid`, `eid`, `heat`, `payload`, `topic`, `received_at`.  
  - String fields like `"time":"3453"` are parsed as milliseconds.  
  - On receipt, the server updates the **resume** value for the next **start** to match the device.

Use `EventSource('/api/mqtt/stream')` and listen for the event names above.

---

### Prerequisites

- Go 1.22 or newer.
- Optional: a `.env` file in the project directory to override defaults.

### Configuration

Environment variables (and optional `.env`):

| Variable | Default | Notes |
|----------|---------|--------|
| `APP_NAME` | `ESP32 Backend (Go)` | |
| `DEBUG` | `false` | |
| `PORT` | `8000` | HTTP listen port |
| `BLE_NAME_PREFIX` | `ESP-Setup-` | BLE name filter |
| `MOCK_BLE` | `true` | Mock BLE devices and WiFi provisioning |
| `WIFI_SERVICE_UUID` | (see `main.go`) | WiFi provisioning service |
| `WIFI_CONFIG_CHAR_UUID` | (see `main.go`) | WiFi config characteristic |
| `WIFI_STATUS_CHAR_UUID` | (see `main.go`) | Status / indication characteristic |
| `MQTT_ENABLED` | `true` | |
| `MQTT_BROKER_HOST` | `localhost` | |
| `MQTT_BROKER_PORT` | `1883` | |
| `MQTT_PUBLISH_TOPIC` | `stopwatch/{device_id}/cmd` | `{device_id}` placeholder supported |
| `MQTT_SUBSCRIBE_TOPIC` | `stopwatch/+/data` | Live stopwatch JSON |
| `MQTT_SUBSCRIBE_HEALTH_TOPIC` | `stopwatch/+/health` | Device heartbeat |
| `MQTT_SUBSCRIBE_TIME_TOPIC` | `stopwatch/+/time` | Final timing after stop/reset |
| `MQTT_USERNAME` | `""` | MQTT auth (optional) |
| `MQTT_PASSWORD` | `""` | MQTT auth (optional) |
| `MQTT_CLIENT_ID` | auto | Empty → random client id |
| `MQTT_CONNECT_TIMEOUT_SEC` | `5` | Initial broker connect wait |

With `MOCK_BLE=true` (default), you get fake BLE devices and simulated WiFi provisioning without hardware.

### Running the server

From this directory (`stopwatch-golang`):

```bash
go mod tidy
go run ./...
```

The server listens on `http://localhost:8000` by default (or `PORT` from the environment / `.env`).

#### Example HTTP requests

```bash
curl http://localhost:8000/health
curl http://localhost:8000/api/config
curl http://localhost:8000/api/devices
```

```bash
curl -X POST "http://localhost:8000/api/devices/FAKE:01/wifi" \
  -H "Content-Type: application/json" \
  -d '{"ssid":"MyWifi","password":"secret"}'
```

```bash
curl -X POST "http://localhost:8000/api/mqtt/devices/FAKE:01/start"
```

In real BLE mode, replace `FAKE:01` with the `id` from `GET /api/devices`.

### Real BLE implementation

`main.go` defines a `BLEService` interface and provides:

- **`mockBLEService`**: matches Python `MOCK_BLE` behavior.
- **`realBLEService`**: discovers devices by `BLE_NAME_PREFIX`, connects, writes WiFi JSON `{"ssid","pass"}` to the config characteristic, and reads status/indications.

To use real BLE:

```bash
set MOCK_BLE=false
go run ./...
```

(On Linux/macOS use `export MOCK_BLE=false`.)
