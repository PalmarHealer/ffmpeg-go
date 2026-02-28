package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"

	pb "github.com/palmarhealer/ffmpeg-go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const chunkSize = 64 * 1024

var (
	activeJobs int64
	token      string
	maxJobs    int64
	port       string
)

type server struct {
	pb.UnimplementedFFmpegProxyServer
}

func (s *server) Health(_ context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{
		ActiveJobs: int32(atomic.LoadInt64(&activeJobs)),
		MaxJobs:    int32(maxJobs),
	}, nil
}

func (s *server) Process(stream pb.FFmpegProxy_ProcessServer) error {
	// 1. Receive first message: meta
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	meta, ok := first.Payload.(*pb.ProcessRequest_Meta)
	if !ok {
		return status.Error(codes.InvalidArgument, "first message must be meta")
	}

	// 2. Authenticate
	if meta.Meta.Token != token {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	// 3. Check job limit
	if maxJobs > 0 && atomic.LoadInt64(&activeJobs) >= maxJobs {
		return status.Error(codes.ResourceExhausted, "max jobs reached")
	}

	args := meta.Meta.Args
	if len(args) == 0 {
		return status.Error(codes.InvalidArgument, "no args provided")
	}

	// Security: whitelist only ffmpeg and ffprobe
	cmd0 := args[0]
	if cmd0 != "ffmpeg" && cmd0 != "ffprobe" {
		return status.Error(codes.PermissionDenied, "only ffmpeg and ffprobe are allowed")
	}

	// 4. Setup context tied to stream
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	atomic.AddInt64(&activeJobs, 1)
	defer atomic.AddInt64(&activeJobs, -1)

	// 5. Build command – args[0] is the binary, args[1:] are its arguments
	cmd := exec.CommandContext(ctx, cmd0, args[1:]...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start ffmpeg: %v", err)
	}
	defer cmd.Wait()

	// 6. Goroutine: stream.Recv() → stdin pipe
	recvErr := make(chan error, 1)
	go func() {
		defer stdinPipe.Close()
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				recvErr <- nil
				return
			}
			if err != nil {
				recvErr <- err
				return
			}
			chunk, ok := req.Payload.(*pb.ProcessRequest_Chunk)
			if !ok {
				continue
			}
			if _, err := stdinPipe.Write(chunk.Chunk); err != nil {
				recvErr <- err
				return
			}
		}
	}()

	// 7. Goroutine: stdout pipe → stream.Send(chunk)
	sendErr := make(chan error, 1)
	go func() {
		buf := make([]byte, chunkSize)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if sendErr2 := stream.Send(&pb.ProcessResponse{
					Payload: &pb.ProcessResponse_Chunk{Chunk: chunk},
				}); sendErr2 != nil {
					sendErr <- sendErr2
					return
				}
			}
			if err == io.EOF {
				sendErr <- nil
				return
			}
			if err != nil {
				sendErr <- err
				return
			}
		}
	}()

	// Wait for both goroutines
	var firstErr error
	for i := 0; i < 2; i++ {
		select {
		case err := <-recvErr:
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
		case err := <-sendErr:
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
		}
	}

	if firstErr != nil {
		return status.Errorf(codes.Internal, "stream error: %v", firstErr)
	}

	// 8. Wait for ffmpeg to finish
	waitErr := cmd.Wait()
	exitCode := int32(0)
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			exitCode = 1
		}
	}

	// Send result
	return stream.Send(&pb.ProcessResponse{
		Payload: &pb.ProcessResponse_Result{
			Result: &pb.ProcessResult{
				ExitCode: exitCode,
				Stderr:   stderrBuf.String(),
			},
		},
	})
}

func loadEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadEnv()

	port = os.Getenv("PORT")
	if port == "" {
		port = "50051"
	}
	token = os.Getenv("TOKEN")
	maxJobsStr := os.Getenv("MAX_JOBS")
	if maxJobsStr != "" {
		if v, err := strconv.ParseInt(maxJobsStr, 10, 64); err == nil {
			maxJobs = v
		}
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterFFmpegProxyServer(grpcServer, &server{})

	log.Printf("ffmpeg-remote server listening on :%s (max_jobs=%d)", port, maxJobs)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
