package main

import (
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

	// Priority 4 (lowest): /etc/ffmpeg-proxy/config.json
	if c, ok := loadConfigFile("/etc/ffmpeg-proxy/config.json"); ok {
		cfg = mergeConfig(cfg, c)
	}

	// Priority 3: ~/.config/ffmpeg-proxy/config.json
	if home, err := os.UserHomeDir(); err == nil {
		if c, ok := loadConfigFile(filepath.Join(home, ".config", "ffmpeg-proxy", "config.json")); ok {
			cfg = mergeConfig(cfg, c)
		}
	}

	// Priority 2: $FFMPEG_PROXY_CONFIG
	if envPath := os.Getenv("FFMPEG_PROXY_CONFIG"); envPath != "" {
		if c, ok := loadConfigFile(envPath); ok {
			cfg = mergeConfig(cfg, c)
		}
	}

	// Priority 1 (highest): ./ffmpeg-proxy.json
	if c, ok := loadConfigFile("ffmpeg-proxy.json"); ok {
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

// sanitizeArgs replaces the input file path with pipe:0 and output file with pipe:1
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
	return result
}

func runRemote(cfg Config, args []string) error {
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
	stream, err := client.Process(nil)
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
	var lastStderr string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		switch p := resp.Payload.(type) {
		case *pb.ProcessResponse_Chunk:
			if _, err := outF.Write(p.Chunk); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
		case *pb.ProcessResponse_Result:
			lastExitCode = p.Result.ExitCode
			lastStderr = p.Result.Stderr
		}
	}

	// Wait for send goroutine
	if err := <-sendErrCh; err != nil {
		return fmt.Errorf("send: %w", err)
	}

	if lastStderr != "" {
		fmt.Fprint(os.Stderr, lastStderr)
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
		fmt.Fprintf(os.Stderr, "ffmpeg-proxy: remote failed: %v\n", err)
		if cfg.Fallback == "never" {
			os.Exit(1)
		}
	}

	// Fallback
	runFallback(cfg.FallbackBin, args)
}
