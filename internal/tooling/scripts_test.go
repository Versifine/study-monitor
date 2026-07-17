package tooling

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const nestedScriptTest = "EXAM_MONITOR_NESTED_SCRIPT_TEST"

func TestBuildScriptMarksUntrackedGoInputDirtyAndUsesLocalToolchain(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	fixture := filepath.Join(t.TempDir(), "repository")
	run(t, "git", "clone", "--quiet", "--no-hardlinks", "--local", repository, fixture)

	copyFile(t, filepath.Join(repository, "scripts", "build.ps1"), filepath.Join(fixture, "scripts", "build.ps1"))
	if status := strings.TrimSpace(run(t, "git", "-C", fixture, "status", "--porcelain=v1", "--untracked-files=all")); status != "" {
		run(t, "git", "-C", fixture, "add", "scripts/build.ps1")
		if staged := strings.TrimSpace(run(t, "git", "-C", fixture, "diff", "--cached", "--name-only")); staged != "" {
			run(t, "git", "-C", fixture, "config", "user.name", "Exam Monitor Tests")
			run(t, "git", "-C", fixture, "config", "user.email", "tests@example.invalid")
			run(t, "git", "-C", fixture, "commit", "--quiet", "-m", "test fixture: current build script")
		}
	}
	commit := strings.TrimSpace(run(t, "git", "-C", fixture, "rev-parse", "HEAD"))
	if status := strings.TrimSpace(run(t, "git", "-C", fixture, "status", "--porcelain=v1", "--untracked-files=all")); status != "" {
		t.Fatalf("fixture must be clean before the untracked input is added: %s", status)
	}

	probe := filepath.Join(fixture, "cmd", "exam-monitor", "untracked_build_probe.go")
	if err := os.WriteFile(probe, []byte("package main\n\nvar untrackedBuildProbe = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := run(t, "git", "-C", fixture, "status", "--porcelain=v1", "--untracked-files=all"); !strings.Contains(status, "untracked_build_probe.go") {
		t.Fatalf("untracked build input is not visible to Git status: %s", status)
	}

	outputDirectory := filepath.Join(t.TempDir(), "build output")
	wrapper := writePowerShellWrapper(t, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:GOTOOLCHAIN = 'invalid-unless-build-script-overrides-it'
$before = $env:GOTOOLCHAIN
$binary = (& '%s' -OutputDirectory '%s' | Select-Object -Last 1)
if ($env:GOTOOLCHAIN -ne $before) { throw 'build.ps1 did not restore GOTOOLCHAIN' }
& $binary --version
if ($LASTEXITCODE -ne 0) { throw 'built binary version check failed' }
`, quotePowerShell(filepath.Join(fixture, "scripts", "build.ps1")), quotePowerShell(outputDirectory)))

	output := runPowerShell(t, wrapper, nil)
	var build struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal([]byte(lastNonemptyLine(output)), &build); err != nil {
		t.Fatalf("decode build version: %v\noutput:\n%s", err, output)
	}
	if want := commit + "-dirty"; build.Commit != want {
		t.Fatalf("build commit = %q, want %q", build.Commit, want)
	}
}

func TestTestAndDevScriptsUseLocalToolchainAndRestoreEnvironment(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	wrapper := writePowerShellWrapper(t, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:GOTOOLCHAIN = 'invalid-unless-script-overrides-it'
$before = $env:GOTOOLCHAIN
$env:%s = '1'
& '%s'
if ($env:GOTOOLCHAIN -ne $before) { throw 'test.ps1 did not restore GOTOOLCHAIN' }
& '%s' -Version
if ($env:GOTOOLCHAIN -ne $before) { throw 'dev.ps1 did not restore GOTOOLCHAIN' }
`, nestedScriptTest, quotePowerShell(filepath.Join(repository, "scripts", "test.ps1")), quotePowerShell(filepath.Join(repository, "scripts", "dev.ps1"))))

	runPowerShell(t, wrapper, nil)
}

func requireOuterWindowsTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("M0 PowerShell contracts are Windows-specific")
	}
	if os.Getenv(nestedScriptTest) != "" {
		t.Skip("skip script recursion")
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePowerShellWrapper(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "verify.ps1")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runPowerShell(t *testing.T, script string, environment []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("PowerShell timed out: %v\n%s", ctx.Err(), output)
		}
		t.Fatalf("PowerShell failed: %v\n%s", err, output)
	}
	return string(output)
}

func run(t *testing.T, name string, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("%s timed out: %v\n%s", name, ctx.Err(), output)
		}
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

func quotePowerShell(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func lastNonemptyLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
