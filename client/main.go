package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pb "github.com/palmarhealer/ffmpeg-go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const chunkSize = 64 * 1024

type Config struct {
	ServerURL   string `json:"server_url"`
	Token       string `json:"token"`
	FallbackBin string `json:"fallback_bin"`
	Fallback    string `json:"fallback"`
}

func defaultConfig() Config {
	return Config{
		FallbackBin: "ffmpeg.real",
		Fallback:    "always",
	}
}

func mergeConfig(base, overlay Config) Config {
	if overlay.ServerURL != "" {
		base.ServerURL = overlay.ServerURL
	}
	if overlay.Token != "" {
		base.Token = overlay.Token
	}
	if overlay.FallbackBin != "" {
		base.FallbackBin = overlay.FallbackBin
	}
	if overlay.Fallback != "" {
		base.Fallback = overlay.Fallback
	}
	return base
}

func loadConfigFile(path string) (Config, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, false
	}
	defer f.Close()
	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return Config{}, false
	}
	return cfg, true
}

func loadConfig() Config {
	cfg := defaultConfig()

	// Priority 4 (lowest): /etc/ffmpeg-remote/config.json
	if c, ok := loadConfigFile("/etc/ffmpeg-remote/config.json"); ok {
		cfg = mergeConfig(cfg, c)
	}

	// Priority 3: ~/.config/ffmpeg-remote/config.json
	if home, err := os.UserHomeDir(); err == nil {
		if c, ok := loadConfigFile(filepath.Join(home, ".config", "ffmpeg-remote", "config.json")); ok {
			cfg = mergeConfig(cfg, c)
		}
	}

	// Priority 2: $FFMPEG_REMOTE_CONFIG
	if envPath := os.Getenv("FFMPEG_REMOTE_CONFIG"); envPath != "" {
		if c, ok := loadConfigFile(envPath); ok {
			cfg = mergeConfig(cfg, c)
		}
	}

	// Priority 1 (highest): ./ffmpeg-remote.json
	if c, ok := loadConfigFile("ffmpeg-remote.json"); ok {
		cfg = mergeConfig(cfg, c)
	}

	return cfg
}

func runFallback(bin string, args []string) {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "ffmpeg-remote: fallback failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// detectInputFile finds the argument after -i
func detectInputFile(args []string) string {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// detectOutputFile returns the last argument if it doesn't start with "-"
// and is not the value of the -i flag.
func detectOutputFile(args []string) string {
	if len(args) == 0 {
		return ""
	}
	last := args[len(args)-1]
	if strings.HasPrefix(last, "-") {
		return ""
	}
	// The last arg is a value of -i, not an output file
	if len(args) >= 2 && args[len(args)-2] == "-i" {
		return ""
	}
	return last
}

// sanitizeArgs replaces the input file path with pipe:0 and output file with pipe:1.
// For muxers that require seekable output (mp4, mov, …) it injects
// -movflags +frag_keyframe+empty_moov so the stream can be written to a pipe.
func sanitizeArgs(args []string, inputFile, outputFile string) []string {
	result := make([]string, len(args))
	copy(result, args)
	for i, a := range result {
		if a == inputFile && i > 0 && result[i-1] == "-i" {
			result[i] = "pipe:0"
		} else if a == outputFile && i == len(result)-1 {
			result[i] = "pipe:1"
		}
	}
	if pipeFormatNeedsFragFlags(result) {
		last := result[len(result)-1]
		result = append(result[:len(result)-1], "-movflags", "+frag_keyframe+empty_moov", last)
	}
	return result
}

// pipeFormatNeedsFragFlags reports whether args use a seekable-only muxer
// (mp4, mov, …) with pipe:1 as output, requiring fragmentation flags.
func pipeFormatNeedsFragFlags(args []string) bool {
	if len(args) == 0 || args[len(args)-1] != "pipe:1" {
		return false
	}
	for i, a := range args {
		if a == "-f" && i+1 < len(args) {
			switch args[i+1] {
			case "mp4", "mov", "m4v", "3gp", "3g2":
				return true
			}
		}
	}
	return false
}

// isHLSMode returns true when args contain -f hls.
func isHLSMode(args []string) bool {
	for i, a := range args {
		if a == "-f" && i+1 < len(args) && args[i+1] == "hls" {
			return true
		}
	}
	return false
}

// extractOutputDir returns the directory where HLS output files should be written.
// It prefers the directory of -hls_segment_filename, falling back to the directory
// of the last non-flag argument (the m3u8 playlist path).
func extractOutputDir(args []string) string {
	for i, a := range args {
		if a == "-hls_segment_filename" && i+1 < len(args) {
			path := strings.TrimPrefix(args[i+1], "file:")
			return filepath.Dir(path)
		}
	}
	// Fall back to dir of last non-flag arg (playlist)
	for i := len(args) - 1; i >= 0; i-- {
		if !strings.HasPrefix(args[i], "-") {
			if i == 0 || args[i-1] != "-i" {
				path := strings.TrimPrefix(args[i], "file:")
				return filepath.Dir(path)
			}
		}
	}
	return "."
}

// sanitizeHLSArgs transforms args for exec mode on the server:
//   - replaces -i value with pipe:0 (strips file: URI prefix)
//   - strips directory from -hls_segment_filename value (server uses cmd.Dir)
//   - strips directory from -hls_fmp4_init_filename if absolute
//   - strips directory from the final playlist argument
//
// Returns args with "ffmpeg" prepended as args[0].
func sanitizeHLSArgs(args []string) []string {
	result := make([]string, len(args))
	copy(result, args)

	// Find the index of the last non-flag, non-i-value arg (the playlist output)
	lastOutputIdx := -1
	for i := len(result) - 1; i >= 0; i-- {
		if !strings.HasPrefix(result[i], "-") {
			if i == 0 || result[i-1] != "-i" {
				lastOutputIdx = i
				break
			}
		}
	}

	for i := 0; i < len(result); i++ {
		switch result[i] {
		case "-i":
			if i+1 < len(result) {
				i++
				result[i] = "pipe:0"
			}
		case "-hls_segment_filename":
			if i+1 < len(result) {
				i++
				val := strings.TrimPrefix(result[i], "file:")
				result[i] = filepath.Base(val)
			}
		case "-hls_fmp4_init_filename":
			if i+1 < len(result) {
				i++
				val := strings.TrimPrefix(result[i], "file:")
				if filepath.IsAbs(val) {
					result[i] = filepath.Base(val)
				} else {
					result[i] = val
				}
			}
		}
	}

	// Strip dir from the playlist output arg
	if lastOutputIdx >= 0 {
		val := strings.TrimPrefix(result[lastOutputIdx], "file:")
		result[lastOutputIdx] = filepath.Base(val)
	}

	return append([]string{"ffmpeg"}, result...)
}

// runRemoteExec handles HLS / exec mode: FFmpeg writes to a temp dir on the server
// and each output file is streamed back as FileChunk messages.
func runRemoteExec(cfg Config, args []string) error {
	inputFile := detectInputFile(args)
	if inputFile == "" {
		return fmt.Errorf("no input file detected (-i flag missing)")
	}

	outputDir := extractOutputDir(args)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}

	// Strip file: URI prefix when opening locally
	actualInputPath := strings.TrimPrefix(inputFile, "file:")
	inF, err := os.Open(actualInputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer inF.Close()

	conn, err := grpc.Dial(cfg.ServerURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc dial: %w", err)
	}
	defer conn.Close()

	client := pb.NewFFmpegProxyClient(conn)
	stream, err := client.Process(context.Background())
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	sanitized := sanitizeHLSArgs(args)

	// Send goroutine: meta first, then input file in chunks
	sendErrCh := make(chan error, 1)
	go func() {
		defer stream.CloseSend()

		if err := stream.Send(&pb.ProcessRequest{
			Payload: &pb.ProcessRequest_Meta{
				Meta: &pb.ProcessMeta{
					Token:    cfg.Token,
					Args:     sanitized,
					ExecMode: true,
				},
			},
		}); err != nil {
			sendErrCh <- err
			return
		}

		buf := make([]byte, chunkSize)
		for {
			n, err := inF.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if sendErr := stream.Send(&pb.ProcessRequest{
					Payload: &pb.ProcessRequest_Chunk{Chunk: chunk},
				}); sendErr != nil {
					sendErrCh <- sendErr
					return
				}
			}
			if err == io.EOF {
				sendErrCh <- nil
				return
			}
			if err != nil {
				sendErrCh <- err
				return
			}
		}
	}()

	// Receive responses: FileChunk messages write named files into outputDir.
	// Using os.Create on each new file sequence so m3u8 rewrites truncate correctly.
	openFiles := make(map[string]*os.File)
	defer func() {
		for _, f := range openFiles {
			f.Close()
		}
	}()

	var lastExitCode int32
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		switch p := resp.Payload.(type) {
		case *pb.ProcessResponse_StderrChunk:
			os.Stderr.Write(p.StderrChunk) //nolint:errcheck
		case *pb.ProcessResponse_FileChunk:
			fc := p.FileChunk
			name := filepath.Base(fc.Filename) // ensure basename only
			if !fc.Eof {
				f, ok := openFiles[name]
				if !ok {
					// os.Create truncates – correct for m3u8 rewrites
					f, err = os.Create(filepath.Join(outputDir, name))
					if err != nil {
						return fmt.Errorf("create %s: %w", name, err)
					}
					openFiles[name] = f
				}
				if _, err := f.Write(fc.Data); err != nil {
					return fmt.Errorf("write %s: %w", name, err)
				}
			} else {
				// EOF marker: close and evict so next sequence re-creates
				if f, ok := openFiles[name]; ok {
					f.Close()
					delete(openFiles, name)
				}
			}
		case *pb.ProcessResponse_Result:
			lastExitCode = p.Result.ExitCode
		}
	}

	if err := <-sendErrCh; err != nil {
		return fmt.Errorf("send: %w", err)
	}

	if lastExitCode != 0 {
		os.Exit(int(lastExitCode))
	}
	return nil
}

func runRemote(cfg Config, args []string) error {
	// Route HLS commands to exec mode
	if isHLSMode(args) {
		return runRemoteExec(cfg, args)
	}

	inputFile := detectInputFile(args)
	outputFile := detectOutputFile(args)

	if inputFile == "" {
		return fmt.Errorf("no input file detected (-i flag missing)")
	}
	if outputFile == "" {
		return fmt.Errorf("no output file detected")
	}

	// Open input file before connecting
	inF, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer inF.Close()

	// Open/create output file
	outF, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer outF.Close()

	conn, err := grpc.Dial(cfg.ServerURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc dial: %w", err)
	}
	defer conn.Close()

	client := pb.NewFFmpegProxyClient(conn)
	stream, err := client.Process(context.Background())
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Build sanitized args: binary name + modified args
	sanitized := sanitizeArgs(args, inputFile, outputFile)
	// Prepend "ffmpeg" as the command name
	fullArgs := append([]string{"ffmpeg"}, sanitized...)

	// Send in a goroutine: meta first, then file chunks
	sendErrCh := make(chan error, 1)
	go func() {
		defer stream.CloseSend()

		if err := stream.Send(&pb.ProcessRequest{
			Payload: &pb.ProcessRequest_Meta{
				Meta: &pb.ProcessMeta{
					Token: cfg.Token,
					Args:  fullArgs,
				},
			},
		}); err != nil {
			sendErrCh <- err
			return
		}

		buf := make([]byte, chunkSize)
		for {
			n, err := inF.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if sendErr := stream.Send(&pb.ProcessRequest{
					Payload: &pb.ProcessRequest_Chunk{Chunk: chunk},
				}); sendErr != nil {
					sendErrCh <- sendErr
					return
				}
			}
			if err == io.EOF {
				sendErrCh <- nil
				return
			}
			if err != nil {
				sendErrCh <- err
				return
			}
		}
	}()

	// Receive responses
	var lastExitCode int32
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		switch p := resp.Payload.(type) {
		case *pb.ProcessResponse_StderrChunk:
			os.Stderr.Write(p.StderrChunk) //nolint:errcheck
		case *pb.ProcessResponse_Chunk:
			if _, err := outF.Write(p.Chunk); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
		case *pb.ProcessResponse_Result:
			lastExitCode = p.Result.ExitCode
		}
	}

	// Wait for send goroutine
	if err := <-sendErrCh; err != nil {
		return fmt.Errorf("send: %w", err)
	}

	if lastExitCode != 0 {
		os.Exit(int(lastExitCode))
	}
	return nil
}

func main() {
	args := os.Args[1:]
	cfg := loadConfig()

	if cfg.ServerURL != "" {
		err := runRemote(cfg, args)
		if err == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "ffmpeg-remote: remote failed: %v\n", err)
		if cfg.Fallback == "never" {
			os.Exit(1)
		}
	}

	// Fallback
	runFallback(cfg.FallbackBin, args)
}
