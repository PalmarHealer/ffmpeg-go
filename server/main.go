package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
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

	// Route to exec mode if requested
	if meta.Meta.ExecMode {
		return s.processExec(stream, args, meta.Meta.DirectInputPath)
	}

	// 4. Setup context tied to stream
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	atomic.AddInt64(&activeJobs, 1)
	defer atomic.AddInt64(&activeJobs, -1)

	// 5. Build command
	cmd := exec.CommandContext(ctx, cmd0, args[1:]...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdout pipe: %v", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start ffmpeg: %v", err)
	}

	// Mutex to serialize stream.Send across stdout and stderr goroutines
	var sendMu sync.Mutex
	safeSend := func(resp *pb.ProcessResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(resp)
	}

	// 6. Goroutine: stream.Recv() → stdin pipe
	recvDone := make(chan struct{})
	recvClientErr := make(chan error, 1)
	go func() {
		defer close(recvDone)
		defer stdinPipe.Close()
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				recvClientErr <- err
				cancel()
				return
			}
			chunk, ok := req.Payload.(*pb.ProcessRequest_Chunk)
			if !ok {
				continue
			}
			if _, err := stdinPipe.Write(chunk.Chunk); err != nil {
				// FFmpeg closed stdin early; stop reading input immediately.
				// Do NOT drain – that would block for minutes on large files.
				// The gRPC framework discards remaining incoming messages when
				// the server-side RPC returns.
				return
			}
		}
	}()

	// 7. Goroutine: stdout pipe → stream.Send(chunk)
	stdoutDone := make(chan error, 1)
	go func() {
		buf := make([]byte, chunkSize)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if sendErr := safeSend(&pb.ProcessResponse{
					Payload: &pb.ProcessResponse_Chunk{Chunk: chunk},
				}); sendErr != nil {
					stdoutDone <- sendErr
					cancel()
					return
				}
			}
			if err == io.EOF {
				stdoutDone <- nil
				return
			}
			if err != nil {
				stdoutDone <- err
				cancel()
				return
			}
		}
	}()

	// 8. Goroutine: stderr pipe → stream.Send(StderrChunk) for live progress
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		buf := make([]byte, chunkSize)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				safeSend(&pb.ProcessResponse{ //nolint:errcheck
					Payload: &pb.ProcessResponse_StderrChunk{StderrChunk: chunk},
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for stdout (signals ffmpeg has finished writing output)
	if err := <-stdoutDone; err != nil {
		<-recvDone
		<-stderrDone
		return status.Errorf(codes.Internal, "send: %v", err)
	}

	// Wait for stderr relay to flush
	<-stderrDone

	select {
	case err := <-recvClientErr:
		<-recvDone
		return status.Errorf(codes.Internal, "recv: %v", err)
	default:
	}

	<-recvDone

	waitErr := cmd.Wait()
	exitCode := int32(0)
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			exitCode = 1
		}
	}

	return stream.Send(&pb.ProcessResponse{
		Payload: &pb.ProcessResponse_Result{
			Result: &pb.ProcessResult{ExitCode: exitCode},
		},
	})
}

// sendFileToStream reads path in chunks and sends it as FileChunk messages via send.
// The final message has Eof=true and no data. Updates sentFiles with the basename.
// If the file is empty (premature Create event on Windows) the send is skipped.
func sendFileToStream(send func(*pb.ProcessResponse) error, path string, sentFiles map[string]bool) {
	filename := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	buf := make([]byte, chunkSize)
	sentAny := false
	for {
		n, err := f.Read(buf)
		if n > 0 {
			sentAny = true
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if sendErr := send(&pb.ProcessResponse{
				Payload: &pb.ProcessResponse_FileChunk{
					FileChunk: &pb.FileChunk{
						Filename: filename,
						Data:     chunk,
						Eof:      false,
					},
				},
			}); sendErr != nil {
				return
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
	}

	if !sentAny {
		return
	}

	send(&pb.ProcessResponse{ //nolint:errcheck
		Payload: &pb.ProcessResponse_FileChunk{
			FileChunk: &pb.FileChunk{
				Filename: filename,
				Eof:      true,
			},
		},
	})
	sentFiles[filename] = true
}

// processExec handles HLS / multi-file output mode.
// When directInputPath is non-empty, FFmpeg reads the input file directly from
// that path (shared mount); no stdin pipe is created and no recv goroutine runs.
func (s *server) processExec(stream pb.FFmpegProxy_ProcessServer, args []string, directInputPath string) error {
	tmpDir, err := os.MkdirTemp("", "ffmpeg-exec-*")
	if err != nil {
		return status.Errorf(codes.Internal, "mkdirtemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if args[0] != "ffmpeg" && args[0] != "ffprobe" {
		return status.Error(codes.PermissionDenied, "only ffmpeg and ffprobe are allowed")
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	atomic.AddInt64(&activeJobs, 1)
	defer atomic.AddInt64(&activeJobs, -1)

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = tmpDir
	cmd.Stdout = io.Discard

	recvDone := make(chan struct{})
	recvClientErr := make(chan error, 1)

	if directInputPath != "" {
		// Direct mode: FFmpeg opens the file itself; no stdin needed.
		close(recvDone)
	} else {
		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			return status.Errorf(codes.Internal, "stdin pipe: %v", err)
		}
		// Goroutine A: recv input → stdinPipe
		go func() {
			defer close(recvDone)
			defer stdinPipe.Close()
			for {
				req, err := stream.Recv()
				if err == io.EOF {
					return
				}
				if err != nil {
					recvClientErr <- err
					cancel()
					return
				}
				chunk, ok := req.Payload.(*pb.ProcessRequest_Chunk)
				if !ok {
					continue
				}
				if _, err := stdinPipe.Write(chunk.Chunk); err != nil {
					// FFmpeg closed stdin early; stop reading immediately.
					return
				}
			}
		}()
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stderr pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start ffmpeg: %v", err)
	}

	// Mutex to serialize sends from watcher goroutine and stderr goroutine
	var sendMu sync.Mutex
	safeSend := func(resp *pb.ProcessResponse) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(resp)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cancel()
		cmd.Wait() //nolint:errcheck
		return status.Errorf(codes.Internal, "fsnotify: %v", err)
	}
	defer watcher.Close()

	if err := watcher.Add(tmpDir); err != nil {
		cancel()
		cmd.Wait() //nolint:errcheck
		return status.Errorf(codes.Internal, "watcher.Add: %v", err)
	}

	ffmpegDone := make(chan struct{})
	watcherDone := make(chan struct{})
	stderrDone := make(chan struct{})

	// Goroutine B: watcher relay → safeSend(FileChunk)
	sentFiles := make(map[string]bool)
	go func() {
		defer close(watcherDone)
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				name := filepath.Base(event.Name)
				isMP4 := strings.HasSuffix(name, ".mp4")
				isM3U8 := strings.HasSuffix(name, ".m3u8")

				if isMP4 && (event.Has(fsnotify.Create) || event.Has(fsnotify.Rename)) {
					if !sentFiles[name] {
						sendFileToStream(safeSend, event.Name, sentFiles)
					}
				} else if isM3U8 && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
					sendFileToStream(safeSend, event.Name, sentFiles)
				}

			case <-ffmpegDone:
				entries, _ := os.ReadDir(tmpDir)
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					name := e.Name()
					path := filepath.Join(tmpDir, name)
					if strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".m3u8") {
						sendFileToStream(safeSend, path, sentFiles)
					}
				}
				return

			case <-watcher.Errors:
				return
			}
		}
	}()

	// Goroutine C: stderr pipe → safeSend(StderrChunk) for live progress
	go func() {
		defer close(stderrDone)
		buf := make([]byte, chunkSize)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				safeSend(&pb.ProcessResponse{ //nolint:errcheck
					Payload: &pb.ProcessResponse_StderrChunk{StderrChunk: chunk},
				})
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for all input, then stderr EOF (= ffmpeg exited), then reap
	<-recvDone
	<-stderrDone

	waitErr := cmd.Wait()
	close(ffmpegDone)
	<-watcherDone

	select {
	case err := <-recvClientErr:
		return status.Errorf(codes.Internal, "recv: %v", err)
	default:
	}

	exitCode := int32(0)
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			exitCode = 1
		}
	}

	return stream.Send(&pb.ProcessResponse{
		Payload: &pb.ProcessResponse_Result{
			Result: &pb.ProcessResult{ExitCode: exitCode},
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
