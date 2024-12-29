# kube-mqtt-mirror

A Kubernetes admission webhook that automatically mirrors container images to a local registry using MQTT or other messaging backends for distributed operation.

## Features

- **Multiple Messaging Backends**:
  - In-memory queue for single instance operation
  - MQTT broker for distributed operation
  - PostgreSQL for distributed operation with persistence

- **Flexible Deployment**:
  - Webhook server with optional TLS
  - Works behind reverse proxies
  - Multi-architecture support (amd64/arm64)

- **Image Mirroring**:
  - Automatic detection of new images via webhook
  - Support for multi-arch images
  - Concurrent downloads with retries
  - Private registry support

## Installation

### Binary Installation

Download the latest release for your platform from the [releases page](https://github.com/andrewl3wis/mqtt_k8s_mirror/releases).

```bash
# Linux AMD64
curl -L -o kube-mqtt-mirror https://github.com/andrewl3wis/mqtt_k8s_mirror/releases/download/v1.0.0/kube-mqtt-mirror_Linux_x86_64.tar.gz
tar xzf kube-mqtt-mirror_Linux_x86_64.tar.gz

# macOS ARM64
curl -L -o kube-mqtt-mirror https://github.com/andrewl3wis/mqtt_k8s_mirror/releases/download/v1.0.0/kube-mqtt-mirror_Darwin_arm64.tar.gz
tar xzf kube-mqtt-mirror_Darwin_arm64.tar.gz
```

### Docker Installation

```bash
# Pull multi-arch image (automatically selects correct architecture)
docker pull ghcr.io/andrewl3wis/mqtt_k8s_mirror/kube-mqtt-mirror:latest

# Or pull specific architecture
docker pull ghcr.io/andrewl3wis/mqtt_k8s_mirror/kube-mqtt-mirror:latest-amd64
docker pull ghcr.io/andrewl3wis/mqtt_k8s_mirror/kube-mqtt-mirror:latest-arm64
```

## Usage

### Basic Usage (Single Instance)

```bash
# Start with in-memory queue (no external dependencies)
kube-mqtt-mirror -webhook -mirror -messaging-type queue
```

### Distributed Operation with MQTT

```bash
# Start webhook server
kube-mqtt-mirror -webhook -messaging-type mqtt -broker tcp://mqtt-server:1883

# Start mirror worker
kube-mqtt-mirror -mirror -messaging-type mqtt -broker tcp://mqtt-server:1883
```

### Distributed Operation with PostgreSQL

```bash
# Start webhook server
kube-mqtt-mirror -webhook -messaging-type postgres \
  -broker localhost:5432 \
  -username user \
  -password pass

# Start mirror worker
kube-mqtt-mirror -mirror -messaging-type postgres \
  -broker localhost:5432 \
  -username user \
  -password pass
```

### TLS Configuration

TLS is automatically enabled when certificate and key files are provided:

```bash
# Direct access with TLS
kube-mqtt-mirror -webhook -mirror -messaging-type queue \
  -tls-cert server.crt \
  -tls-key server.key

# Behind reverse proxy (no TLS)
kube-mqtt-mirror -webhook -mirror -messaging-type queue
```

## Command Line Options

```
Usage: kube-mqtt-mirror [options]

Options:
  -webhook            Enable webhook server to detect pod creation events
  -mirror             Enable image mirroring service to process download requests
  -messaging-type     Messaging backend type (queue, mqtt, postgres)
  -broker            Messaging broker address
  -topic             Topic/channel name for messaging (default: image/download)
  -username          Username for MQTT/PostgreSQL authentication
  -password          Password for MQTT/PostgreSQL authentication
  -local-registry    Address of local Docker registry (default: localhost:5000)
  -webhook-port      Port for webhook server (default: 8443)
  -tls-cert          Path to TLS certificate (enables TLS if provided)
  -tls-key           Path to TLS private key (enables TLS if provided)
  -log-file          Log file path (defaults to stdout)
```

## Development

### Prerequisites

- Go 1.21+
- Docker for multi-arch builds

### Building from Source

```bash
# Build binary
go build -o kube-mqtt-mirror

# Run tests
go test -v ./...

# Build Docker images
docker buildx build -f Dockerfile.amd64 -t kube-mqtt-mirror:amd64 .
docker buildx build -f Dockerfile.arm64 -t kube-mqtt-mirror:arm64 .
```

### Release Process

1. Tag a new version:
   ```bash
   git tag -a v1.x.x -m "Release v1.x.x"
   git push origin v1.x.x
   ```

2. GitHub Actions will automatically:
   - Build binaries for all platforms
   - Create Docker images
   - Publish release artifacts

## License

MIT License