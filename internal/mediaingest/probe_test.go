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
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed; scripts/smoke.ps1 performs the target-machine check")
	}
	prober := ExecProber{Path: ffprobePath}
	version, err := prober.Version(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if version != SupportedFFprobeVersion {
		t.Skipf("installed ffprobe version %q is not the pinned M2 version %q", version, SupportedFFprobeVersion)
	}

	encoded, err := os.ReadFile(filepath.Join("..", "..", "testdata", "media", "valid.mp4.b64"))
	if err != nil {
		t.Fatal(err)
	}
	media, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
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
