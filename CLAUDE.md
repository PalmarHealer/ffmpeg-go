# CLAUDE.md – FFmpeg Remote Proxy: Go Server + Client

## Goal

Build two Go programs:

1. **`server/main.go`** – gRPC server that accepts and executes FFmpeg jobs
2. **`client/main.go`** – Transparent drop-in replacement for the local `ffmpeg` binary

Minimal dependencies. Only two external packages allowed: `google.golang.org/grpc` and `google.golang.org/protobuf`.

---

## ⚠️ Core Principle: Everything in RAM – No Disk, No Buffering

This is the single most important requirement:

- The input file is **streamed live from client to server** – byte by byte, never fully loaded into RAM
- FFmpeg starts immediately when the first bytes arrive – **not after the upload finishes**
- FFmpeg output is **streamed live back to the client** while FFmpeg is still transcoding
- **Nothing is ever written to disk** – neither on the server nor the client (except the final output file on the client side)
- Everything flows through in-memory pipes:

```
Client file → gRPC stream (upload) → ffmpeg stdin
                                      ffmpeg stdout → gRPC stream (download) → Client output file
```

### Memory Cleanup

When a job ends – whether success, error, or client disconnect – cleanup must happen immediately:

- Kill the FFmpeg process via context cancellation
- Close all pipes
- Decrement the job counter
- No held references → Go GC cleans up immediately

Implement exclusively with `defer`:

```go
ctx, cancel := context.WithCancel(stream.Context())
defer cancel()

cmd := exec.CommandContext(ctx, "ffmpeg", args...)
atomic.AddInt64(&activeJobs, 1)
defer atomic.AddInt64(&activeJobs, -1)
defer cmd.Wait()
```

---

## Protocol: gRPC Bidirectional Streaming

gRPC bidirectional streaming is the cleanest fit for this use case:

- True full-duplex over a single connection – native to gRPC
- Upload and download run simultaneously with no workarounds
- Exit code and stderr are cleanly transmitted as gRPC metadata
- Runs over HTTP/2 internally – all performance benefits included

---

## Protobuf Definition (`proto/ffmpeg.proto`)

```protobuf
syntax = "proto3";
package ffmpeg;
option go_package = "./proto";

service FFmpegProxy {
  rpc Process(stream ProcessRequest) returns (stream ProcessResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
}

message ProcessRequest {
  oneof payload {
    ProcessMeta meta = 1;   // first message: arguments + token
    bytes chunk = 2;        // all subsequent: input data
  }
}

message ProcessMeta {
  string token = 1;
  repeated string args = 2;
}

message ProcessResponse {
  oneof payload {
    bytes chunk = 1;
    ProcessResult result = 2;
  }
}

message ProcessResult {
  int32 exit_code = 1;
  string stderr = 2;
}

message HealthRequest {}
message HealthResponse {
  int32 active_jobs = 1;
  int32 max_jobs = 2;
}
```

### Stream Flow

```
Client                          Server
  │─── ProcessRequest(meta) ───►│  Authenticate, start FFmpeg
  │─── ProcessRequest(chunk) ──►│  → ffmpeg stdin
  │◄── ProcessResponse(chunk) ──│  ffmpeg stdout →
  │─── ProcessRequest(chunk) ──►│  → ffmpeg stdin
  │◄── ProcessResponse(chunk) ──│  ffmpeg stdout →
  │         ...                 │
  │─── (stream close) ──────────│  ffmpeg stdin EOF
  │◄── ProcessResponse(result) ─│  Exit code + stderr
```

---

## Client (`client/main.go`)

### Behavior

Transparent drop-in replacement for `ffmpeg`. Accepts exactly the same arguments – no changed interface.

```bash
ffmpeg -i input.mkv -vf scale=1280:720 -f mp4 output.mp4
```

### Config Lookup Order

Only `.json` is supported. All found files are loaded and merged – higher priority overrides lower:

| Priority | Path | Purpose |
|----------|------|---------|
| 1 (highest) | `./ffmpeg-proxy.json` | Current directory – container mount |
| 2 | `$FFMPEG_PROXY_CONFIG` | Explicit path via env var |
| 3 | `~/.config/ffmpeg-proxy/config.json` | User config |
| 4 (lowest) | `/etc/ffmpeg-proxy/config.json` | System config |

### Config Format

JSON only. Parsed with `encoding/json` from the stdlib – no external library.

```json
{
  "server_url": "my-server:50051",
  "token": "my-secret-token",
  "fallback_bin": "ffmpeg.real",
  "fallback": "always"
}
```

| Key | Default | Description |
|-----|---------|-------------|
| `server_url` | – | gRPC host:port (no `http://` prefix) |
| `token` | – | Auth token |
| `fallback_bin` | `ffmpeg.real` | Local fallback binary |
| `fallback` | `always` | `always` or `never` |

### Execution Order

1. Load and merge config files in priority order
2. If `server_url` is set: attempt remote execution
    - Detect input file (argument after `-i`)
    - Detect output file (last argument without `-` prefix)
    - Establish gRPC connection (`grpc.WithInsecure()`)
    - First message: `ProcessMeta` with token + sanitized args
    - Stream input file in 64 KB chunks; simultaneously write response chunks to output file
    - Final message `ProcessResult`: mirror stderr, adopt exit code
3. On remote failure:
    - `fallback=always` → run fallback binary with original arguments
    - `fallback=never` → print error to stderr, exit 1
4. If no config or `server_url` is empty: run fallback directly

### Concurrent Send and Receive

```go
go func() {
    defer stream.CloseSend()
    // send meta, then file in chunks
}()

for {
    resp, err := stream.Recv()
    // chunk → output file, or process result
}
```

### Fallback

```go
cmd := exec.Command(cfg.FallbackBin, os.Args[1:]...)
cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
os.Exit(cmd.Run())
```

---

## Server (`server/main.go`)

### Config (env vars or `.env` in the current directory)

```env
PORT=50051
TOKEN=my-secret-token
MAX_JOBS=4
```

### Endpoints

- `Process` – bidirectional gRPC stream
- `Health` – active jobs + max capacity

### Core Logic

```go
func (s *server) Process(stream proto.FFmpegProxy_ProcessServer) error {
    // 1. Receive meta → authenticate → codes.Unauthenticated
    // 2. Check job limit → codes.ResourceExhausted
    // 3. atomic increment + defer decrement
    // 4. exec.CommandContext(stream.Context(), "ffmpeg", args...)
    // 5. cmd.Stdin = pipe, cmd.Stdout = pipe
    // 6. goroutine: stream.Recv() → stdin pipe
    // 7. goroutine: stdout pipe → stream.Send(chunk)
    // 8. cmd.Wait() → stream.Send(result)
}
```

### Security

- Whitelist: only `ffmpeg` and `ffprobe` allowed as the command
- No shell expansion – always use `exec.CommandContext` directly
- `stream.Context()` cancellation → FFmpeg is killed immediately

---

## Build

```bash
# Generate protobuf (one-time)
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
protoc --go_out=. --go-grpc_out=. proto/ffmpeg.proto

# Server
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o server ./server

# Client
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o client ./client
```

---

## CI/CD: GitHub Actions

### Release Workflow (`.github/workflows/release.yml`)

Triggered when a new git tag is pushed (`v*.*.*`).

**Steps:**
1. Install `protoc` + Go
2. Generate protobuf code
3. Build **client** for `linux/amd64` (statically linked)
4. Upload client binary as a GitHub Release artifact (name: `ffmpeg-proxy-client-linux-amd64`)
5. Build **server** as a Docker image
6. Push image to GitHub Container Registry as `ghcr.io/<owner>/<repo>:latest` and `ghcr.io/<owner>/<repo>:<tag>`

```yaml
name: Release

on:
  push:
    tags:
      - 'v*.*.*'

jobs:
  release:
    runs-on: ubuntu-latest
    permissions:
      contents: write        # for GitHub Release
      packages: write        # for ghcr.io push

    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.21'

      - name: Install protoc
        run: |
          sudo apt-get install -y protobuf-compiler
          go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
          go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

      - name: Generate protobuf
        run: protoc --go_out=. --go-grpc_out=. proto/ffmpeg.proto

      - name: Build client (linux/amd64)
        run: |
          GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
            go build -ldflags="-s -w" -o ffmpeg-proxy-client-linux-amd64 ./client

      - name: Create GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          files: ffmpeg-proxy-client-linux-amd64

      - name: Log in to ghcr.io
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and push Docker image
        uses: docker/build-push-action@v5
        with:
          context: .
          push: true
          tags: |
            ghcr.io/${{ github.repository }}:latest
            ghcr.io/${{ github.repository }}:${{ github.ref_name }}
```

---

## Dockerfile (Server)

```dockerfile
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache protobuf protobuf-dev
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN protoc --go_out=. --go-grpc_out=. proto/ffmpeg.proto
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o server ./server

# Runtime: ffmpeg with NVIDIA support
FROM nvidia/cuda:12.3.1-runtime-ubuntu22.04

RUN apt-get update && apt-get install -y \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/server /usr/local/bin/server

EXPOSE 50051
CMD ["server"]
```

---

## Docker Compose (`docker-compose.yml`)

```yaml
services:
  ffmpeg-proxy:
    image: ghcr.io/<owner>/<repo>:latest
    restart: unless-stopped
    ports:
      - "50051:50051"
    env_file:
      - .env
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu, video]
```

`.env` file (do not commit to the repo):
```env
PORT=50051
TOKEN=change-me
MAX_JOBS=4
```

**Host prerequisite:** [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html) must be installed.

```bash
# Start
docker compose up -d

# Logs
docker compose logs -f

# Update to new version
docker compose pull && docker compose up -d
```

---

## File Structure

```
ffmpeg-proxy/
├── CLAUDE.md
├── go.mod
├── go.sum
├── Dockerfile
├── docker-compose.yml
├── .env.example
├── proto/
│   ├── ffmpeg.proto
│   └── (generated .go files – created by CI, do not commit)
├── server/
│   └── main.go
├── client/
│   └── main.go
└── .github/
    └── workflows/
        └── release.yml
```

---

## What NOT to Build

- No intermediate files on disk
- No buffering entire files in RAM
- No raw HTTP/1.1 or HTTP/2 (use gRPC)
- No web UI, no database
- No TLS (handled by reverse proxy / service mesh)
- No retry logic in the client
- No YAML config support

---

## Success Criteria

```bash
# Push a tag → CI builds automatically
git tag v1.0.0 && git push origin v1.0.0

# Result:
# - GitHub Release with ffmpeg-proxy-client-linux-amd64 as download
# - ghcr.io/<owner>/<repo>:v1.0.0 and :latest available

# Start the server
docker compose up -d

# Download the client, use it as ffmpeg
mv /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg.real
curl -L https://github.com/<owner>/<repo>/releases/latest/download/ffmpeg-proxy-client-linux-amd64 \
  -o /usr/local/bin/ffmpeg && chmod +x /usr/local/bin/ffmpeg

# From now on, transparently remote via gRPC with NVIDIA GPU
ffmpeg -i /data/input.mkv -vf scale=1280:720 -f mp4 /data/output.mp4
```