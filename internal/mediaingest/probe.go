package mediaingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type ExecProber struct {
	Path string
}

func (prober ExecProber) Version(ctx context.Context, timeout time.Duration) (string, error) {
	output, err := runBoundedCommand(ctx, timeout, 64<<10, prober.Path, "-version")
	if err != nil {
		return "", &Error{Code: CodeProbeUnavailable, Err: errors.New("ffprobe version check failed")}
	}
	firstLine := strings.SplitN(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n", 2)[0]
	const prefix = "ffprobe version "
	if !strings.HasPrefix(firstLine, prefix) {
		return "", &Error{Code: CodeProbeUnavailable, Err: errors.New("ffprobe version output is invalid")}
	}
	version := strings.Fields(strings.TrimPrefix(firstLine, prefix))
	if len(version) == 0 {
		return "", &Error{Code: CodeProbeUnavailable, Err: errors.New("ffprobe version output is empty")}
	}
	return version[0], nil
}

func (prober ExecProber) Probe(ctx context.Context, mediaPath string, timeout time.Duration) (ProbeInfo, error) {
	output, err := runBoundedCommand(
		ctx,
		timeout,
		1<<20,
		prober.Path,
		"-v", "error",
		"-show_entries", "format=duration,format_name:stream=codec_type,codec_name",
		"-of", "json",
		mediaPath,
	)
	if err != nil {
		var execError *exec.Error
		var pathError *os.PathError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &execError) || errors.As(err, &pathError) {
			return ProbeInfo{}, &Error{Code: CodeProbeUnavailable, Err: errors.New("ffprobe cannot be executed reliably")}
		}
		return ProbeInfo{}, &Error{Code: CodeProbeFailed, Err: errors.New("ffprobe media validation failed")}
	}
	var response struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration   string `json:"duration"`
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&response); err != nil {
		return ProbeInfo{}, &Error{Code: CodeProbeFailed, Err: errors.New("ffprobe JSON output is invalid")}
	}
	durationMS, err := parseDurationMilliseconds(response.Format.Duration)
	if err != nil {
		return ProbeInfo{}, err
	}
	codec := ""
	for _, stream := range response.Streams {
		if stream.CodecType == "video" && stream.CodecName != "" {
			codec = stream.CodecName
			break
		}
	}
	if codec == "" {
		return ProbeInfo{}, &Error{Code: CodeTypeInvalid, Err: errors.New("media does not contain a decodable video stream")}
	}
	if response.Format.FormatName == "" {
		return ProbeInfo{}, &Error{Code: CodeProbeFailed, Err: errors.New("ffprobe format name is missing")}
	}
	return ProbeInfo{
		DurationMS: durationMS,
		CodecName:  codec,
		FormatName: response.Format.FormatName,
		MediaType:  "video",
	}, nil
}

func parseDurationMilliseconds(value string) (int64, error) {
	durationSeconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) || durationSeconds <= 0 {
		return 0, &Error{Code: CodeDurationInvalid, Err: errors.New("ffprobe duration is invalid")}
	}
	durationMS := math.Ceil(durationSeconds * 1000)
	if durationMS >= float64(uint64(1)<<63) {
		return 0, &Error{Code: CodeDurationInvalid, Err: errors.New("ffprobe duration exceeds supported range")}
	}
	return int64(durationMS), nil
}

func runBoundedCommand(parent context.Context, timeout time.Duration, maximum int, name string, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	var output limitedBuffer
	output.remaining = maximum
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, fmt.Errorf("command output exceeds %d bytes", maximum)
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	remaining int
	exceeded  bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > buffer.remaining {
		value = value[:buffer.remaining]
		buffer.exceeded = true
	}
	if len(value) > 0 {
		_, _ = buffer.Buffer.Write(value)
		buffer.remaining -= len(value)
	}
	return original, nil
}
