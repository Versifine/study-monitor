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

const (
	nestedScriptTest        = "EXAM_MONITOR_NESTED_SCRIPT_TEST"
	powerShellScriptTimeout = 180 * time.Second
)

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
$env:GOPROXY = 'invalid-unless-build-script-overrides-it'
$before = $env:GOTOOLCHAIN
$beforeProxy = $env:GOPROXY
$binary = (& '%s' -OutputDirectory '%s' | Select-Object -Last 1)
if ($env:GOTOOLCHAIN -ne $before) { throw 'build.ps1 did not restore GOTOOLCHAIN' }
if ($env:GOPROXY -ne $beforeProxy) { throw 'build.ps1 did not restore GOPROXY' }
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

func TestCoverageSourceDirectoryIsNotIgnored(t *testing.T) {
	repository := repositoryRoot(t)
	command := exec.Command("git", "-C", repository, "check-ignore", "--no-index", "--quiet", "internal/coverage/coverage.go")
	err := command.Run()
	if err == nil {
		t.Fatal("internal/coverage/coverage.go is ignored and would be omitted from a clean checkout")
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 1 {
		t.Fatalf("git check-ignore failed unexpectedly: %v", err)
	}
}

func TestTestAndDevScriptsUseLocalToolchainAndRestoreEnvironment(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	wrapper := writePowerShellWrapper(t, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:GOTOOLCHAIN = 'invalid-unless-script-overrides-it'
$env:GOPROXY = 'invalid-unless-script-overrides-it'
$before = $env:GOTOOLCHAIN
$beforeProxy = $env:GOPROXY
$env:%s = '1'
& '%s'
if ($env:GOTOOLCHAIN -ne $before) { throw 'test.ps1 did not restore GOTOOLCHAIN' }
if ($env:GOPROXY -ne $beforeProxy) { throw 'test.ps1 did not restore GOPROXY' }
& '%s' -Version
if ($env:GOTOOLCHAIN -ne $before) { throw 'dev.ps1 did not restore GOTOOLCHAIN' }
if ($env:GOPROXY -ne $beforeProxy) { throw 'dev.ps1 did not restore GOPROXY' }
`, nestedScriptTest, quotePowerShell(filepath.Join(repository, "scripts", "test.ps1")), quotePowerShell(filepath.Join(repository, "scripts", "dev.ps1"))))

	runPowerShell(t, wrapper, nil)
}

func TestM4OperationalScriptsParseAsPowerShell(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	names := []string{"process-control.ps1", "install.ps1", "uninstall.ps1", "run-supervised.ps1", "backup.ps1", "restore.ps1", "rollback.ps1", "smoke-m4.ps1", "fault-injection.ps1"}
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = "'" + quotePowerShell(filepath.Join(repository, "scripts", name)) + "'"
	}
	wrapper := writePowerShellWrapper(t, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$failed = @()
foreach ($path in @(%s)) {
    $tokens = $null
    $errors = $null
    [void][Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors)
    if ($errors.Count -ne 0) { $failed += "${path}: $($errors -join '; ')" }
}
if ($failed.Count -ne 0) { throw ($failed -join [Environment]::NewLine) }
`, strings.Join(quoted, ",")))
	runPowerShell(t, wrapper, nil)
}

func TestProcessControlIgnoresStaleSupervisorIdentity(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	appRoot := filepath.Join(t.TempDir(), "app root")
	if err := os.MkdirAll(filepath.Join(appRoot, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := writePowerShellWrapper(t, fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$process = Get-Process -Id $PID
$state = [ordered]@{
    schema_version = 1
    status = 'running'
    supervisor_pid = $PID
    supervisor_started_at_utc = $process.StartTime.ToUniversalTime().AddHours(-1).ToString('o')
    supervisor_executable = $process.Path
    crash_times_utc = @()
    updated_at_utc = [DateTime]::UtcNow.ToString('o')
}
$statePath = '%s'
$state | ConvertTo-Json | Set-Content -LiteralPath $statePath -Encoding UTF8
. '%s'
Stop-ExamMonitorManagedProcesses -AppRoot '%s' -TaskName 'ExamMonitor nonexistent stale identity test' -ErrorPrefix 'TEST'
if ($null -eq (Get-Process -Id $PID -ErrorAction SilentlyContinue)) { throw 'stale supervisor identity killed the caller' }
`, quotePowerShell(filepath.Join(appRoot, "state", "supervisor-state.json")), quotePowerShell(filepath.Join(repository, "scripts", "process-control.ps1")), quotePowerShell(appRoot)))

	runPowerShell(t, wrapper, nil)
}

func requireOuterWindowsTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell contracts are Windows-specific")
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
	ctx, cancel := context.WithTimeout(context.Background(), powerShellScriptTimeout)
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
