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

	copyFile(t, filepath.Join(repository, ".gitattributes"), filepath.Join(fixture, ".gitattributes"))
	copyFile(t, filepath.Join(repository, ".gitignore"), filepath.Join(fixture, ".gitignore"))
	copyFile(t, filepath.Join(repository, "scripts", "build.ps1"), filepath.Join(fixture, "scripts", "build.ps1"))
	copyFile(t, filepath.Join(repository, "scripts", "build-web.ps1"), filepath.Join(fixture, "scripts", "build-web.ps1"))
	for _, relative := range []string{
		"package.json", "build.mjs", "src/index.html", "src/styles.css", "src/state.ts", "src/app.ts",
		"dist/index.html", "dist/assets/styles.css", "dist/assets/state.js", "dist/assets/app.js",
	} {
		copyFile(t, filepath.Join(repository, "web", filepath.FromSlash(relative)), filepath.Join(fixture, "web", filepath.FromSlash(relative)))
	}
	if status := strings.TrimSpace(run(t, "git", "-C", fixture, "status", "--porcelain=v1", "--untracked-files=all")); status != "" {
		run(t, "git", "-C", fixture, "add", ".gitattributes", ".gitignore", "scripts/build.ps1", "scripts/build-web.ps1", "web")
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

func TestOperationalScriptsParseAsPowerShell(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	names := []string{"build-web.ps1", "process-control.ps1", "install.ps1", "uninstall.ps1", "run-supervised.ps1", "backup.ps1", "restore.ps1", "rollback.ps1", "smoke-m4.ps1", "fault-injection.ps1", "m6-certification.ps1", "m6-desk-media.ps1"}
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

func TestM6CertificationProfileDeclaresEveryRequiredExerciseOnce(t *testing.T) {
	repository := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(repository, "configs", "m6-certification.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile struct {
		SchemaVersion         int                `json:"schema_version"`
		SampleIntervalMinutes int                `json:"sample_interval_minutes"`
		BackupRPOHours        int                `json:"backup_rpo_hours"`
		Limits                map[string]float64 `json:"limits"`
		RecoveryRTOSeconds    map[string]int     `json:"recovery_rto_seconds"`
		MediaPublisher        struct {
			DeviceName               string `json:"device_name"`
			FFmpegPath               string `json:"ffmpeg_path"`
			CollectorID              string `json:"collector_id"`
			DailyStartLocal          string `json:"daily_start_local"`
			SegmentSeconds           int    `json:"segment_seconds"`
			SegmentCount             int    `json:"segment_count"`
			ClockErrorMS             int64  `json:"clock_error_ms"`
			AcceptanceTimeoutSeconds int    `json:"acceptance_timeout_seconds"`
		} `json:"media_publisher"`
		PlannedExercises []struct {
			Kind   string `json:"kind"`
			RTOKey string `json:"rto_key"`
		} `json:"planned_exercises"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.SchemaVersion != 1 || profile.SampleIntervalMinutes != 5 || profile.BackupRPOHours != 24 {
		t.Fatalf("invalid M6 profile header: %#v", profile)
	}
	for _, name := range []string{"max_cpu_percent", "max_working_set_bytes", "max_private_bytes", "max_handles", "max_threads", "max_goroutines", "max_go_heap_bytes", "max_go_runtime_system_bytes", "max_wal_bytes", "max_staging_bytes", "max_staging_files", "max_log_bytes", "max_data_bytes", "max_backup_bytes", "max_media_ready_backlog", "min_free_bytes"} {
		if profile.Limits[name] <= 0 {
			t.Fatalf("missing or invalid M6 limit %q", name)
		}
	}
	for _, name := range []string{"core_process", "system_reboot", "activitywatch", "media", "backup_restore", "rollback"} {
		if profile.RecoveryRTOSeconds[name] <= 0 {
			t.Fatalf("missing or invalid M6 RTO %q", name)
		}
	}
	if profile.MediaPublisher.DeviceName == "" || !filepath.IsAbs(profile.MediaPublisher.FFmpegPath) || profile.MediaPublisher.CollectorID != "desk.media" || profile.MediaPublisher.DailyStartLocal != "20:00" || profile.MediaPublisher.SegmentSeconds != 300 || profile.MediaPublisher.SegmentCount != 3 || profile.MediaPublisher.ClockErrorMS < 0 || profile.MediaPublisher.AcceptanceTimeoutSeconds < 5 {
		t.Fatalf("invalid M6 media publisher profile: %#v", profile.MediaPublisher)
	}
	counts := map[string]int{}
	for _, exercise := range profile.PlannedExercises {
		counts[exercise.Kind]++
		if profile.RecoveryRTOSeconds[exercise.RTOKey] <= 0 {
			t.Fatalf("M6 exercise %q has invalid RTO key %q", exercise.Kind, exercise.RTOKey)
		}
	}
	for _, name := range []string{"network_disconnect", "process_termination", "system_reboot", "write_interruption", "duplicate_submission", "corrupt_media", "low_disk", "cloud_unavailable", "clock_offset", "backup_restore", "rollback"} {
		if counts[name] != 1 {
			t.Fatalf("M6 exercise %q count=%d", name, counts[name])
		}
	}
}

func TestM6DeskMediaPlanValidatesFrozenInputsWithoutCapturing(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	root := t.TempDir()
	inbox := filepath.Join(root, "media inbox")
	state := filepath.Join(root, "publisher state")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpeg := filepath.Join(root, "ffmpeg.exe")
	if err := os.WriteFile(ffmpeg, []byte("fixed publisher dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf(`
$ffmpeg = '%s'
$output = @(& '%s' -InboxDirectory '%s' -FFmpegPath $ffmpeg -ExpectedFFmpegSHA256 ((Get-FileHash -LiteralPath $ffmpeg -Algorithm SHA256).Hash.ToLowerInvariant()) -DeviceName 'Integrated Camera' -StateDirectory '%s' -CollectorID 'desk.media' -SegmentSeconds 300 -SegmentCount 3 -ClockErrorMS 1000 -AcceptanceTimeoutSeconds 120 -PlanOnly)
$plan = (($output | Out-String).Trim() | ConvertFrom-Json)
if ($plan.status -ne 'planned' -or $plan.segment_seconds -ne 300 -or $plan.segment_count -ne 3 -or $plan.collector_id -ne 'desk.media') { throw ('publisher plan mismatch: ' + ($plan | ConvertTo-Json -Compress)) }
if (@(Get-ChildItem -LiteralPath '%s' -Force).Count -ne 0 -or @(Get-ChildItem -LiteralPath '%s' -Force).Count -ne 0) { throw 'plan-only created runtime artifacts' }
`, quotePowerShell(ffmpeg), quotePowerShell(filepath.Join(repository, "scripts", "m6-desk-media.ps1")), quotePowerShell(inbox), quotePowerShell(state), quotePowerShell(inbox), quotePowerShell(state))
	runPowerShell(t, writePowerShellWrapper(t, wrapper), nil)
}

func TestM6CoverageGateUsesIndependentScheduleAndStrictMediaUsability(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	script := filepath.Join(repository, "scripts", "m6-certification.ps1")
	wrapper := fmt.Sprintf(`
. '%s'
$coverage = @'
{
  "intervals": [
    {"collector_id":"aw","start_utc":"2026-07-20T00:00:00Z","end_utc":"2026-07-20T00:10:00Z","availability":"offline","quality_flags":[]},
    {"collector_id":"media","start_utc":"2026-07-20T12:00:00Z","end_utc":"2026-07-20T12:05:00Z","availability":"covered","quality_flags":[]},
    {"collector_id":"media","start_utc":"2026-07-20T12:05:00Z","end_utc":"2026-07-20T12:10:00Z","availability":"confirmed_idle","quality_flags":[]}
  ],
  "projections": [
    {"collector_id":"aw","status":"fresh"},
    {"collector_id":"media","status":"fresh"}
  ]
}
'@ | ConvertFrom-Json
$collectors = @'
[
  {"id":"aw","kind":"activitywatch","planned_schedule":{"timezone":"Asia/Shanghai","windows":[{"days":["monday"],"start_local":"08:00","end_local":"08:10"}]}},
  {"id":"media","kind":"media","planned_schedule":{"timezone":"Asia/Shanghai","windows":[{"days":["monday"],"start_local":"20:00","end_local":"20:10"}]}}
]
'@ | ConvertFrom-Json
$excluded = @([pscustomobject]@{ start_utc = '2026-07-20T00:04:00Z'; end_utc = '2026-07-20T00:06:00Z' })
$result = @(Measure-Coverage -Coverage $coverage -Collectors $collectors -Excluded $excluded -DayStartUTC ([datetime]'2026-07-19T16:00:00Z'))
if ($result.Count -ne 2) { throw 'unexpected result count' }
$aw = $result | Where-Object collector_id -eq 'aw'
$media = $result | Where-Object collector_id -eq 'media'
if (-not $aw.classification_passed -or $aw.maximum_unexpected_offline_unknown_seconds -ne 240) { throw ('AW gate mismatch: ' + ($aw | ConvertTo-Json -Compress)) }
if (-not $media.classification_passed -or [math]::Abs([double]$media.usable_ratio - 0.5) -gt 0.0001) { throw ('media gate mismatch: ' + ($media | ConvertTo-Json -Compress)) }
`, quotePowerShell(script))
	runPowerShell(t, writePowerShellWrapper(t, wrapper), nil)
}

func TestM6ExerciseRecordCannotClaimPassedBeyondFrozenRTO(t *testing.T) {
	requireOuterWindowsTest(t)
	repository := repositoryRoot(t)
	script := filepath.Join(repository, "scripts", "m6-certification.ps1")
	records := t.TempDir()
	wrapper := fmt.Sprintf(`
. '%s'
function Assert-FrozenInputs { param($Manifest) }
$now = [DateTime]::UtcNow
$manifest = [pscustomobject]@{
  paths = [pscustomobject]@{ certification_directory = '%s' }
  planned_exercises = @([pscustomobject]@{
    id = 'day03-process'; kind = 'process_termination'; rto_key = 'core_process'; rto_seconds = 90
    start_utc = $now.AddMinutes(-1).ToString('o'); end_utc = $now.AddMinutes(1).ToString('o')
  })
}
$RecordKind = 'process_termination'
$RecordStatus = 'passed'
$RecordDetail = 'measured recovery exceeded RTO'
$RecordDurationSeconds = 91
$record = Invoke-Record -Manifest $manifest
if ($record.status -ne 'failed' -or $record.error_code -ne 'M6_EXERCISE_RTO_MISSED') { throw ('RTO claim accepted: ' + ($record | ConvertTo-Json -Compress)) }
if (-not (Test-Path -LiteralPath (Join-Path '%s' 'violations.jsonl'))) { throw 'RTO violation missing' }
`, quotePowerShell(script), quotePowerShell(records), quotePowerShell(records))
	runPowerShell(t, writePowerShellWrapper(t, wrapper), nil)
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
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
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
