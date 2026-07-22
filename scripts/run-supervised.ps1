[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$AppRoot,
    [int]$MaxRestarts = 5,
    [int]$CrashWindowSeconds = 600,
    [int]$InitialBackoffSeconds = 1,
    [int]$MaximumBackoffSeconds = 30
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-State {
    param([Collections.IDictionary]$Value)
    $stateDirectory = Join-Path $AppRoot 'state'
    [void](New-Item -ItemType Directory -Path $stateDirectory -Force)
    $Value['release_version'] = $releaseVersion
    $Value['supervisor_pid'] = $PID
    $Value['supervisor_started_at_utc'] = $supervisorStartedAtUTC
    $Value['supervisor_executable'] = $supervisorExecutable
    $target = Join-Path $stateDirectory 'supervisor-state.json'
    $temporary = "$target.$([guid]::NewGuid().ToString('N')).tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 6), (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $target -Force
}

if (-not [IO.Path]::IsPathRooted($AppRoot) -or $MaxRestarts -lt 1 -or $MaxRestarts -gt 20 -or $CrashWindowSeconds -lt 60 -or $MaximumBackoffSeconds -lt $InitialBackoffSeconds) {
    throw 'SUPERVISOR_ARGUMENT_INVALID'
}

$pointerPath = Join-Path $AppRoot 'current.json'
if (-not (Test-Path -LiteralPath $pointerPath -PathType Leaf)) { throw 'SUPERVISOR_CURRENT_RELEASE_MISSING' }
$pointer = Get-Content -LiteralPath $pointerPath -Raw -Encoding UTF8 | ConvertFrom-Json
$binaryPath = Join-Path $pointer.release_directory 'exam-monitor.exe'
$configPath = $pointer.config_path
if (-not (Test-Path -LiteralPath $binaryPath -PathType Leaf) -or -not (Test-Path -LiteralPath $configPath -PathType Leaf)) { throw 'SUPERVISOR_RELEASE_INVALID' }
$releaseVersion = [string]$pointer.version
$supervisorProcess = Get-Process -Id $PID
$supervisorStartedAtUTC = $supervisorProcess.StartTime.ToUniversalTime().ToString('o')
$supervisorExecutable = [IO.Path]::GetFullPath([string]$supervisorProcess.Path)

$statePath = Join-Path $AppRoot 'state\supervisor-state.json'
$crashes = @()
if (Test-Path -LiteralPath $statePath -PathType Leaf) {
    try {
        $previousState = Get-Content -LiteralPath $statePath -Raw -Encoding UTF8 | ConvertFrom-Json
        if ([string]$previousState.release_version -eq $releaseVersion) { $crashes = @($previousState.crash_times_utc) }
    } catch { $crashes = @() }
}

while ($true) {
    $now = [DateTime]::UtcNow
    $cutoff = $now.AddSeconds(-$CrashWindowSeconds)
    $crashes = @($crashes | Where-Object { [DateTime]::Parse($_).ToUniversalTime() -ge $cutoff })
    if ($crashes.Count -ge $MaxRestarts) {
        Write-State -Value ([ordered]@{ schema_version = 1; status = 'degraded'; error_code = 'SUPERVISOR_CRASH_LOOP'; crash_times_utc = $crashes; updated_at_utc = $now.ToString('o') })
        exit 3
    }

    $started = [DateTime]::UtcNow
    $configArgument = '--config="' + $configPath + '"'
    $process = Start-Process -FilePath $binaryPath -ArgumentList $configArgument -WindowStyle Hidden -PassThru
    Write-State -Value ([ordered]@{ schema_version = 1; status = 'running'; child_pid = $process.Id; crash_times_utc = $crashes; updated_at_utc = $started.ToString('o') })
    $process.WaitForExit()
    $exitCode = $process.ExitCode
    $runtime = [DateTime]::UtcNow - $started
    if ($exitCode -eq 0) {
        Write-State -Value ([ordered]@{ schema_version = 1; status = 'stopped'; exit_code = 0; crash_times_utc = @(); updated_at_utc = [DateTime]::UtcNow.ToString('o') })
        exit 0
    }
    if ($runtime.TotalSeconds -ge $CrashWindowSeconds) { $crashes = @() }
    $crashes += [DateTime]::UtcNow.ToString('o')
    Write-State -Value ([ordered]@{ schema_version = 1; status = 'restarting'; exit_code = $exitCode; error_code = 'SUPERVISOR_CHILD_EXIT'; crash_times_utc = $crashes; updated_at_utc = [DateTime]::UtcNow.ToString('o') })
    $backoff = [Math]::Min($MaximumBackoffSeconds, $InitialBackoffSeconds * [Math]::Pow(2, [Math]::Max(0, $crashes.Count - 1)))
    Start-Sleep -Seconds ([int]$backoff)
}
