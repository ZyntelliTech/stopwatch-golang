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
| MQTT Broker Port | `8883` (MQTTS; plain MQTT `1883` if TLS disabled) |
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
| `MQTT_BROKER_PORT` | `1883` | broker port (`8883` for MQTTS) |
| `MQTT_TLS_CA_FILE` | `""` | PEM CA path; when set, connects with `ssl://` and verifies the broker cert (e.g. `/etc/mosquitto/certs/ca.crt` on Raspberry Pi) |
| `MQTT_TLS_SERVER_NAME` | `MQTT_BROKER_HOST` | TLS SNI / cert hostname when it differs from the broker host |
| `MQTT_TLS_ALLOW_LEGACY_CN` | `true` | Accept Mosquitto certs that only set Common Name (no SAN). Still verifies the CA chain and that CN matches `MQTT_TLS_SERVER_NAME` / broker host. Set `false` after reissuing the broker cert with SANs. |
| `MQTT_PUBLISH_TOPIC` | `stopwatch/{device_id}/cmd` | supports `{device_id}` |
| `MQTT_CMD_PAYLOAD_PLAIN` | `false` | if `true`, publish `start`/`stop`/`reset` as plain text instead of JSON |
| `MQTT_SUBSCRIBE_TOPIC` | `stopwatch/+/data` | live stopwatch feed |
| `MQTT_SUBSCRIBE_HEALTH_TOPIC` | `stopwatch/+/health` | heartbeat feed |
| `MQTT_SUBSCRIBE_TIME_TOPIC` | `stopwatch/+/time` | time result feed |
| `MQTT_USERNAME` | `""` | optional auth |
| `MQTT_PASSWORD` | `""` | optional auth |
| `MQTT_CLIENT_ID` | auto | random when empty |
| `MQTT_CONNECT_TIMEOUT_SEC` | `5` | connect timeout |

When `MQTT_TLS_CA_FILE` is set, the backend uses **`ssl://`** instead of **`tcp://`**. Check connection status with `GET /api/config` (`mqtt_connected`, `mqtt_tls`).

---

## MQTTS (TLS) Setup

This section covers enabling **MQTT over TLS (MQTTS)** on a Raspberry Pi running Mosquitto, then pointing the Go backend at it. Firmware is built with `WIFI_MQTT_USE_TLS=1` and port **8883** by default.

### 1) Generate certificates on the Pi (Mosquitto)

Paths below match the common Mosquitto layout (`/etc/mosquitto/certs/`). Run on the Pi as a user that can write there (often `sudo`).

**Create a CA** (once):

```bash
sudo mkdir -p /etc/mosquitto/certs
cd /etc/mosquitto/certs

sudo openssl genrsa -out ca.key 4096
sudo openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -out ca.crt -subj "/CN=SwimBrain MQTT CA"
```

**Create a server certificate** with **Subject Alternative Names (SANs)** — recommended for Go and modern TLS:

```bash
sudo openssl genrsa -out server.key 2048
sudo openssl req -new -key server.key -out server.csr \
  -subj "/CN=swimbrain.local"

sudo tee /tmp/mqtt-san.ext >/dev/null <<'EOF'
subjectAltName=DNS:swimbrain.local,DNS:localhost,IP:127.0.0.1,IP:192.168.4.1
EOF

sudo openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key \
  -CAcreateserial -out server.crt -days 825 -sha256 \
  -extfile /tmp/mqtt-san.ext

sudo rm server.csr /tmp/mqtt-san.ext
sudo chmod 640 server.key ca.key
sudo chown root:mosquitto server.key server.crt ca.crt ca.key
```

If you already have a **CN-only** server cert (no SAN), the Go backend can still connect with `MQTT_TLS_ALLOW_LEGACY_CN=true` (default). Prefer reissuing with SANs and then set `MQTT_TLS_ALLOW_LEGACY_CN=false`.

**Inspect the cert:**

```bash
openssl x509 -in /etc/mosquitto/certs/server.crt -noout -subject -ext subjectAltName
```

### 2) Configure Mosquitto listeners

Edit `/etc/mosquitto/mosquitto.conf` (or a file under `/etc/mosquitto/conf.d/`):

```conf
# Plain MQTT (optional; disable in production if you only want TLS)
# listener 1883
# protocol mqtt

# MQTTS
listener 8883
protocol mqtt
cafile /etc/mosquitto/certs/ca.crt
certfile /etc/mosquitto/certs/server.crt
keyfile /etc/mosquitto/certs/server.key
require_certificate false
```

Restart Mosquitto:

```bash
sudo systemctl restart mosquitto
sudo systemctl status mosquitto
```

**Test from the Pi** with the CA file:

```bash
mosquitto_sub -h swimbrain.local -p 8883 \
  --cafile /etc/mosquitto/certs/ca.crt \
  -t 'stopwatch/#' -v
```

### 3) Configure the Go backend (`.env`)

On the Pi (where `ca.crt` exists), set:

```env
MQTT_ENABLED=true
MQTT_BROKER_HOST=swimbrain.local
MQTT_BROKER_PORT=8883
MQTT_TLS_CA_FILE=/etc/mosquitto/certs/ca.crt
MQTT_TLS_SERVER_NAME=swimbrain.local
MQTT_TLS_ALLOW_LEGACY_CN=true
```

| Variable | Purpose |
| --- | --- |
| `MQTT_TLS_CA_FILE` | Enables MQTTS; PEM path to the CA that signed the broker cert |
| `MQTT_TLS_SERVER_NAME` | Hostname for TLS verification / SNI (must match cert CN or SAN) |
| `MQTT_TLS_ALLOW_LEGACY_CN` | `true` if the broker cert has only CN and no SAN (common with older Mosquitto scripts) |

Restart the Go backend, then verify:

```bash
curl http://localhost:8000/api/config
```

Expected:

```json
{
  "mqtt_tls": true,
  "mqtt_connected": true,
  "mqtt_broker_host": "swimbrain.local",
  "mqtt_broker_port": 8883,
  "mqtt_tls_ca_file": "/etc/mosquitto/certs/ca.crt",
  "mqtt_tls_allow_legacy_cn": true
}
```

Successful startup logs look like:

```text
MQTT TLS enabled (CA: /etc/mosquitto/certs/ca.crt, server_name: swimbrain.local, legacy_cn=true)
Connected to MQTT broker ssl://swimbrain.local:8883
Subscribed to MQTT topic stopwatch/+/health
Subscribed to MQTT topic stopwatch/+/time
```

With `DEBUG=true`, each received device message is logged as `MQTT recv topic=...`.

### 4) Firmware / devices

ESP32 firmware embeds the same CA (`mqtt_ca.pem`) and connects to `swimbrain.local:8883` when `WIFI_MQTT_USE_TLS=1`. After changing the CA or broker cert, rebuild and OTA firmware so devices trust the new CA.

### 5) Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `tls: failed to verify certificate: x509: certificate relies on legacy Common Name field, use SANs instead` | Broker cert has CN only, no SAN | Set `MQTT_TLS_ALLOW_LEGACY_CN=true`, or reissue `server.crt` with SANs (step 1) |
| `mqtt_connected: false`, CN / hostname errors | `MQTT_BROKER_HOST` or `MQTT_TLS_SERVER_NAME` does not match cert | Align names with `openssl x509 -in server.crt -noout -subject -ext subjectAltName` |
| `MQTT TLS config error: read MQTT TLS CA file` | Backend not running on the Pi, or wrong path | Use the Pi path `/etc/mosquitto/certs/ca.crt`; on Windows dev, copy `ca.crt` locally or leave `MQTT_TLS_CA_FILE` empty for plain MQTT |
| Devices on broker, backend sees nothing | Backend not connected or not subscribed | Confirm `mqtt_connected: true`; check Mosquitto ACLs allow this client to subscribe to `stopwatch/#` |
| Connection timeout | Wrong port or firewall | Confirm listener `8883`; run `ss -tlnp` and check port 8883 is listening |

**Disable TLS** (local dev only): remove or comment out `MQTT_TLS_CA_FILE` and use port `1883`.

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
