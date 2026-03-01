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
	"path/filepath"
	"strconv"
	"strings"
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
		return s.processExec(stream, args)
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

	// 6. Goroutine: stream.Recv() → stdin pipe
	// If stdin write fails (ffmpeg exited early), we stop feeding but do NOT
	// treat it as a fatal stream error – let stdout drain and report exit code.
	recvDone := make(chan struct{})
	recvClientErr := make(chan error, 1) // only set on gRPC recv failure
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
				// FFmpeg closed its stdin (exited early); drain remaining
				// stream bytes so the client's send goroutine unblocks.
				for {
					_, e := stream.Recv()
					if e != nil {
						break
					}
				}
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
					cancel()
					return
				}
			}
			if err == io.EOF {
				sendErr <- nil
				return
			}
			if err != nil {
				sendErr <- err
				cancel()
				return
			}
		}
	}()

	// Wait for stdout to finish (primary signal that ffmpeg is done writing)
	if err := <-sendErr; err != nil {
		<-recvDone
		return status.Errorf(codes.Internal, "send: %v", err)
	}

	// Check for client-side recv error
	select {
	case err := <-recvClientErr:
		<-recvDone
		return status.Errorf(codes.Internal, "recv: %v", err)
	default:
	}

	<-recvDone

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

// sendFileToStream reads path and sends it as a sequence of FileChunk messages.
// The final message has Eof=true and no data. Updates sentFiles with the basename.
// If the file is empty (e.g. caught by a premature Create event on Windows before
// FFmpeg has written any data) the send is skipped and sentFiles is not updated.
func sendFileToStream(stream pb.FFmpegProxy_ProcessServer, path string, sentFiles map[string]bool) {
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
			if sendErr := stream.Send(&pb.ProcessResponse{
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

	// Skip empty files – FFmpeg may not have written anything yet (Windows Create event race)
	if !sentAny {
		return
	}

	// EOF marker
	stream.Send(&pb.ProcessResponse{ //nolint:errcheck
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
// FFmpeg writes to a temp dir; fsnotify relays completed files back over gRPC.
func (s *server) processExec(stream pb.FFmpegProxy_ProcessServer, args []string) error {
	// 1. Create temp dir
	tmpDir, err := os.MkdirTemp("", "ffmpeg-exec-*")
	if err != nil {
		return status.Errorf(codes.Internal, "mkdirtemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// 2. Validate whitelist (defensive; already checked in Process)
	if args[0] != "ffmpeg" && args[0] != "ffprobe" {
		return status.Error(codes.PermissionDenied, "only ffmpeg and ffprobe are allowed")
	}

	// 3. Context + job counter
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	atomic.AddInt64(&activeJobs, 1)
	defer atomic.AddInt64(&activeJobs, -1)

	// 4. Build command
	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = tmpDir
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return status.Errorf(codes.Internal, "stdin pipe: %v", err)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.Internal, "start ffmpeg: %v", err)
	}

	// 5. Start fsnotify watcher
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

	// Channels
	recvDone := make(chan struct{})
	recvClientErr := make(chan error, 1)
	ffmpegDone := make(chan struct{})
	watcherDone := make(chan struct{})

	// 6. Goroutine A: recv input → stdinPipe
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
				// FFmpeg closed stdin early; drain remaining stream bytes
				for {
					_, e := stream.Recv()
					if e != nil {
						break
					}
				}
				return
			}
		}
	}()

	// 7. Goroutine B: watcher relay → stream.Send(FileChunk)
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
						sendFileToStream(stream, event.Name, sentFiles)
					}
				} else if isM3U8 && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
					// m3u8 sent on every update; content changes
					sendFileToStream(stream, event.Name, sentFiles)
				}

			case <-ffmpegDone:
				// Final scan: send all mp4 and m3u8 files.
				// MP4s are always re-sent regardless of sentFiles: on Windows,
				// fsnotify Create fires before FFmpeg has written data, so the
				// earlier watcher-triggered send may have been empty/partial.
				// The client uses os.Create (truncate) so duplicate sends are safe.
				entries, _ := os.ReadDir(tmpDir)
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					name := e.Name()
					path := filepath.Join(tmpDir, name)
					if strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".m3u8") {
						sendFileToStream(stream, path, sentFiles)
					}
				}
				return

			case <-watcher.Errors:
				return
			}
		}
	}()

	// 8. Wait for input, then FFmpeg, then watcher
	<-recvDone
	waitErr := cmd.Wait()
	close(ffmpegDone)
	<-watcherDone

	// Check for client recv error
	select {
	case err := <-recvClientErr:
		return status.Errorf(codes.Internal, "recv: %v", err)
	default:
	}

	// 9. Determine exit code and send result
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
