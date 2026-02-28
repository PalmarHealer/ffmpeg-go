FROM golang:1.24-alpine AS builder

RUN apk add --no-cache protobuf protobuf-dev
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN protoc --go_out=. --go-grpc_out=. proto/ffmpeg.proto
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/server ./server

# Runtime: ffmpeg with NVIDIA support
FROM nvidia/cuda:12.3.1-runtime-ubuntu22.04

RUN apt-get update && apt-get install -y \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/server /usr/local/bin/server

EXPOSE 50051
CMD ["/usr/local/bin/server"]
