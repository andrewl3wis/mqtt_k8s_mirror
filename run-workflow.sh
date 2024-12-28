#!/bin/bash

# Create artifacts directory
mkdir -p /tmp/artifacts

# Run the test job
act -j test-sqlite \
  -P ubuntu-22.04=ghcr.io/catthehacker/ubuntu:act-22.04 \
  --artifact-server-path /tmp/artifacts

# Show manifest list after test completes
echo "Checking nginx manifest list..."
curl -s -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
  http://localhost:5000/v2/nginx/manifests/latest | jq .
