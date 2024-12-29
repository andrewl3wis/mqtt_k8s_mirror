# Kubernetes Image Mirror

A webhook-based tool for automatically mirroring container images to a local registry.

## Features

- Webhook-based image detection from Kubernetes pods
- Multiple messaging backends for scalability
- Concurrent image downloads with retries
- Support for multi-arch images
- TLS support for webhook endpoint

## Installation

```bash
go install github.com/andrewl3wis/kube-mqtt-mirror@latest
```

Or build from source:

```bash
git clone https://github.com/andrewl3wis/kube-mqtt-mirror.git
cd kube-mqtt-mirror
go build
```

## Usage

The tool can run in two modes:
- Webhook mode: Listens for pod creation events
- Mirror mode: Processes image mirror requests

You can enable either or both modes:

```bash
# Run both webhook and mirror
./kube-mqtt-mirror -webhook -mirror

# Run only webhook
./kube-mqtt-mirror -webhook

# Run only mirror
./kube-mqtt-mirror -mirror
```

### Messaging Backends

Choose from three messaging backends:

1. Queue (Local):
```bash
./kube-mqtt-mirror -messaging-type queue -broker queue/messages.json
# Or use in-memory queue:
./kube-mqtt-mirror -messaging-type queue
```

2. MQTT (Distributed):
```bash
./kube-mqtt-mirror -messaging-type mqtt -broker tcp://localhost:1883
```

3. PostgreSQL (Distributed):
```bash
./kube-mqtt-mirror -messaging-type postgres -broker localhost:5432 \
  -username myuser -password mypass
```

### Full Example

```bash
./kube-mqtt-mirror \
  -webhook \
  -mirror \
  -messaging-type mqtt \
  -broker tcp://localhost:1883 \
  -local-registry localhost:5000 \
  -webhook-port 8443 \
  -tls-cert server.crt \
  -tls-key server.key
```

## Configuration

### Command Line Flags

- `-webhook`: Enable webhook server
- `-mirror`: Enable image mirroring
- `-messaging-type`: Messaging backend (queue, mqtt, postgres)
- `-broker`: Messaging broker address
- `-topic`: Message topic (default: image/download)
- `-username`: Messaging username (if required)
- `-password`: Messaging password (if required)
- `-local-registry`: Local registry address (default: localhost:5000)
- `-webhook-port`: Webhook server port (default: 8443)
- `-insecure-registries`: Allow insecure registry connections
- `-tls-cert`: Path to TLS certificate
- `-tls-key`: Path to TLS private key
- `-log-file`: Path to log file (default: stdout)

### Kubernetes Setup

1. Create TLS certificate:
```bash
openssl req -x509 -newkey rsa:2048 \
  -keyout server.key \
  -out server.crt \
  -days 365 \
  -nodes \
  -subj "/CN=webhook-server"
```

2. Create webhook configuration:
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: image-mirror-webhook
webhooks:
- name: mirror.k8s.io
  rules:
  - apiGroups: [""]
    apiVersions: ["v1"]
    operations: ["CREATE"]
    resources: ["pods"]
    scope: "Namespaced"
  clientConfig:
    url: "https://webhook-server:8443/webhook"
    caBundle: "<base64-encoded-ca-cert>"
  admissionReviewVersions: ["v1"]
  sideEffects: None
  timeoutSeconds: 5
```

3. Example Pod Configurations:

Single container:
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
  - name: nginx
    image: nginx:latest
```

Multiple containers:
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: web-stack
spec:
  containers:
  - name: web
    image: nginx:latest
  - name: cache
    image: redis:alpine
  - name: db
    image: postgres:14
```

The webhook will automatically mirror all container images from any pod created in the cluster.

## Development

### Running Tests

```bash
# Run all tests
go test ./...

# Run integration tests
go test -tags=integration ./...
```

### Adding New Messaging Backend

1. Implement the messaging.Client interface:
```go
type Client interface {
    Connect(ctx context.Context) error
    Disconnect()
    Publish(image string) error
    Subscribe(handler func(ImageRequest) error) error
}
```

2. Add your implementation to main.go's messaging type switch.

## License

MIT License