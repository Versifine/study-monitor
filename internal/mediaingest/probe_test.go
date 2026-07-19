package mediaingest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecProberAgainstPinnedFixture(t *testing.T) {
	ffprobePath := requirePinnedFFprobe(t)
	prober := ExecProber{Path: ffprobePath}
	media := loadPinnedMediaFixture(t)
	digest := sha256.Sum256(media)
	if got, want := hex.EncodeToString(digest[:]), "346f4da339e0f6f91e1785436c74f97a42b87392e6f4939b2fc01b5e12f442d8"; got != want {
		t.Fatalf("fixture SHA-256 = %s, want %s", got, want)
	}

	mediaPath := filepath.Join(t.TempDir(), "valid.mp4")
	if err := os.WriteFile(mediaPath, media, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := prober.Probe(context.Background(), mediaPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if info.MediaType != "video" || info.CodecName != "h264" || info.DurationMS != 1000 {
		t.Fatalf("probe info = %#v", info)
	}
}

func TestDurationMillisecondsNeverRoundsDown(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{value: "0.0004", want: 1},
		{value: "1.0000", want: 1000},
		{value: "600.0004", want: 600001},
	}
	for _, test := range tests {
		got, err := parseDurationMilliseconds(test.value)
		if err != nil || got != test.want {
			t.Fatalf("parseDurationMilliseconds(%q) = %d, %v; want %d", test.value, got, err, test.want)
		}
	}
}

func TestExecProberClassifiesMissingExecutableAsUnavailable(t *testing.T) {
	prober := ExecProber{Path: filepath.Join(t.TempDir(), "missing-ffprobe")}
	_, err := prober.Probe(context.Background(), filepath.Join(t.TempDir(), "media.mp4"), time.Second)
	if ErrorCode(err) != CodeProbeUnavailable {
		t.Fatalf("Probe() error = %v, want %s", err, CodeProbeUnavailable)
	}
}

func requirePinnedFFprobe(t *testing.T) string {
	t.Helper()
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed; scripts/smoke.ps1 performs the target-machine check")
	}
	version, err := (ExecProber{Path: ffprobePath}).Version(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if version != SupportedFFprobeVersion {
		t.Skipf("installed ffprobe version %q is not the pinned M2 version %q", version, SupportedFFprobeVersion)
	}
	return ffprobePath
}

func loadPinnedMediaFixture(t *testing.T) []byte {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("..", "..", "testdata", "media", "valid.mp4.b64"))
	if err != nil {
		t.Fatal(err)
	}
	media, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	return media
}
