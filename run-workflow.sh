#!/bin/bash

# Create artifacts directory
mkdir -p /tmp/artifacts

# Use different ports for local act runs to avoid conflicts
export ACT_WEBHOOK_PORT=9443
export ACT_POSTGRES_PORT=6432
export ACT_MQTT_PORT=2883
export ACT_REGISTRY_PORT_SQLITE=5000

# Run only the build and sqlite test jobs
act -j amd64-build -j test-sqlite \
  -P ubuntu-22.04=ghcr.io/catthehacker/ubuntu:act-22.04 \
  --artifact-server-path /tmp/artifacts \
  -e WEBHOOK_PORT=9443 \
  -e REGISTRY_PORT=5000 \
  -e POSTGRES_PORT=6432 \
  -e MQTT_PORT=2883

# Show manifest list after test completes
echo "Checking nginx manifest list..."
curl -s -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  http://localhost:5000/v2/nginx/manifests/latest | jq .
