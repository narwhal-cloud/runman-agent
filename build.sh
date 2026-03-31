
GOOS=linux GOARCH=amd64 go build -tags containers_image_openpgp -ldflags="-s -w -X main.version=v0.1.0" -o runman-agent-linux-amd64 .

GOOS=linux GOARCH=arm64 go build -tags containers_image_openpgp -ldflags="-s -w -X main.version=v0.1.0" -o runman-agent-linux-arm64 .

