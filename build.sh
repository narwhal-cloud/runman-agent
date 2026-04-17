#!/bin/bash

# 从 git tag 或 commit 读取版本号
VERSION=$(git describe --tags --abbrev=0 2>/dev/null || git rev-parse --short HEAD)
if [ -z "$VERSION" ]; then
    VERSION="dev"
fi

echo "Building version: $VERSION"

GOOS=linux GOARCH=amd64 go build -tags containers_image_openpgp -ldflags="-s -w -X main.version=$VERSION" -o runman-agent-linux-amd64 .

GOOS=linux GOARCH=arm64 go build -tags containers_image_openpgp -ldflags="-s -w -X main.version=$VERSION" -o runman-agent-linux-arm64 .
