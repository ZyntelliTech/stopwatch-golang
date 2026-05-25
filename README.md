## Stopwatch Go Backend + Firmware Contract

This repository contains the Go backend (`main.go`) and web dashboard (`web/index.html`) that integrate with stopwatch firmware over MQTT.

This README is aligned to the firmware protocol in `stopwatch-espidf/Firmware/Readme.md` and documents the expected MQTT contract, API endpoints, and runtime configuration.

---

## Network Setup

Current firmware ships with fixed network credentials:

| Parameter | Value |
| --- | --- |
| WiFi SSID | `SwimBrain` |
| WiFi Password | `swimtime2026` |
| MQTT Broker IP | `192.168.4.1` |
| MQTT Broker Port | `1883` |
| Auto-reconnect | Enabled |

---

## Device Identity

Each watch uses its WiFi MAC address as its permanent identity.

- MQTT client ID is the device MAC.
- MAC is used in topic paths (for example, `stopwatch/AABBCCDDEEFF/time`).
- Lane/watch assignment is not baked into firmware; the backend/brain pairs devices dynamically.

This allows identical firmware across all watches and quick replacement of failed units.

---

## MQTT Contract

### 1) Time payload (watch -> brain)

- **Topic:** `stopwatch/{mac}/time`
- **QoS:** 1
- **When:** every button press (split or finish)

Example payload:

```json
{
  "time": 62150,
  "type": 1,
  "eid": 7,
  "heat": 1
}
```

Fields:

| Field | Type | Notes |
| --- | --- | --- |
| `time` | string or number | Milliseconds since start |
| `type` | int | `0` = split, `1` = finish |
| `eid` | int | Event ID from race state push |
| `heat` | int | Heat from race state push |

### 2) Heartbeat payload (watch -> brain)

- **Topic:** `stopwatch/{mac}/health`
- **QoS:** 0

Example payload:

```json
{
  "bat": 87,
  "rssi": -42,
  "status": "idle"
}
```

Fields:

| Field | Type | Values |
| --- | --- | --- |
| `bat` | int | `0-100` |
| `rssi` | int | negative dBm |
| `status` | string | `idle`, `timing`, `error` |

### 3) Commands (brain -> watch)

- **Topic:** `stopwatch/{mac}/cmd` (example: `stopwatch/AABBCCDDEEFF/cmd`)
- **QoS:** 1

**Start** (default JSON from the Go backend):

```json
{
  "id": "AABBCCDDEEFF",
  "cmd": "start"
}
```

**Stop:**

```json
{
  "id": "AABBCCDDEEFF",
  "cmd": "stop"
}
```

**Reset:**

```json
{
  "id": "AABBCCDDEEFF",
  "cmd": "reset"
}
```

**OTA** (JSON):

```json
{
  "cmd": "ota",
  "url": "http://192.168.4.1:8080/firmware.bin"
}
```

Timing command fields:

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | Device MAC (same value as in the topic path) |
| `cmd` | string | `start`, `stop`, or `reset` |

OTA fields:

| Field | Type | Notes |
| --- | --- | --- |
| `cmd` | string | Must be `ota` |
| `url` | string | `http://` firmware URL |

Plain-text timing commands (when `MQTT_CMD_PAYLOAD_PLAIN=true`):

```text
start
```

```text
stop
```

```text
reset
```

Plain-text OTA alternatives are listed in the OTA section below.

### 4) Race state push (brain -> all watches)

- **Topic:** `brain/race/state`
- **QoS:** 1

Example payload:

```json
{
  "eid": 12,
  "name": "100 Free",
  "heat": 2,
  "action": "next_heat"
}
```

### 5) Device config push (brain -> one watch)

- **Topic:** `stopwatch/{mac}/config`
- **QoS:** 1

Example payload:

```json
{
  "lane": 3,
  "watch": 1,
  "role": 0
}
```

---

## Complete Topic Map

```text
WATCH -> BRAIN
stopwatch/{mac}/time      (QoS 1)  split/finish time events
stopwatch/{mac}/health    (QoS 0)  heartbeat

BRAIN -> WATCH
stopwatch/{mac}/cmd       (QoS 1)  start/stop/reset/ota
stopwatch/{mac}/config    (QoS 1)  lane/watch/role assignment
brain/race/state          (QoS 1)  event/heat broadcast
```

---

## OTA Firmware Update

OTA is accepted on `stopwatch/{mac}/cmd`.

Supported formats:

1. JSON (recommended):

```json
{
  "cmd": "ota",
  "url": "http://192.168.4.1:8080/firmware.bin"
}
```

2. Plain text:

- `ota:http://192.168.4.1:8080/firmware.bin`
- `ota http://192.168.4.1:8080/firmware.bin`

Notes:

- Current implementation supports `http://` firmware URLs.
- While OTA is in progress, additional OTA requests are rejected.
- On success, firmware switches partition and reboots.

---

## Backend API Reference

Base URL (default): `http://localhost:8000`

### Health and config

- `GET /health` -> `{"status":"ok"}`
- `GET /api/config` -> effective runtime config (MQTT topics, port, flags)

### BLE discovery and WiFi provisioning

- `GET /api/devices`
- `POST /api/devices/{device_id}/wifi`
- `POST /api/devices/wifi/batch`

### MQTT command endpoint

- `POST /api/mqtt/devices/{device_id}/{command}`
- `command` = `start | stop | reset`
- Publishes to configured `MQTT_PUBLISH_TOPIC` (default `stopwatch/{device_id}/cmd`)

### SSE stream endpoint

- `GET /api/mqtt/stream`
- Event types:
  - `ready`
  - `stopwatch`
  - `heartbeat`
  - `server_tick`
  - `timing`

The dashboard at `/` consumes this stream and renders per-device stopwatch cards.

---

## Configuration

Optional `.env` values:

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_NAME` | `ESP32 Backend (Go)` | app label |
| `DEBUG` | `false` | debug mode |
| `PORT` | `8000` | HTTP port |
| `BLE_NAME_PREFIX` | `ESP-Setup-` | BLE name filter |
| `MOCK_BLE` | `true` | mock BLE mode |
| `WIFI_SERVICE_UUID` | from `main.go` | BLE service UUID |
| `WIFI_CONFIG_CHAR_UUID` | from `main.go` | BLE write char UUID |
| `WIFI_STATUS_CHAR_UUID` | from `main.go` | BLE status char UUID |
| `MQTT_ENABLED` | `true` | MQTT toggle |
| `MQTT_BROKER_HOST` | `localhost` | broker host |
| `MQTT_BROKER_PORT` | `1883` | broker port |
| `MQTT_PUBLISH_TOPIC` | `stopwatch/{device_id}/cmd` | supports `{device_id}` |
| `MQTT_CMD_PAYLOAD_PLAIN` | `false` | if `true`, publish `start`/`stop`/`reset` as plain text instead of JSON |
| `MQTT_SUBSCRIBE_TOPIC` | `stopwatch/+/data` | live stopwatch feed |
| `MQTT_SUBSCRIBE_HEALTH_TOPIC` | `stopwatch/+/health` | heartbeat feed |
| `MQTT_SUBSCRIBE_TIME_TOPIC` | `stopwatch/+/time` | time result feed |
| `MQTT_USERNAME` | `""` | optional auth |
| `MQTT_PASSWORD` | `""` | optional auth |
| `MQTT_CLIENT_ID` | auto | random when empty |
| `MQTT_CONNECT_TIMEOUT_SEC` | `5` | connect timeout |

---

## Run Locally

```bash
go mod tidy
go run ./...
```

Default URL: `http://localhost:8000`

Quick checks:

```bash
curl http://localhost:8000/health
curl http://localhost:8000/api/config
curl http://localhost:8000/api/devices
```
