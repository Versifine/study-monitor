[CmdletBinding()]
param(
    [string]$AppRoot = (Join-Path $env:LOCALAPPDATA 'ExamMonitor'),
    [string]$TaskName = 'ExamMonitor Recorder Core',
    [switch]$PlanOnly,
    [switch]$NoTaskControl,
    [int]$HealthTimeoutSeconds = 45
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'process-control.ps1')

function Read-Release {
    param([object]$Pointer)
    $releaseRoot = [IO.Path]::GetFullPath((Join-Path $AppRoot 'releases')).TrimEnd('\') + '\'
    $directory = [IO.Path]::GetFullPath([string]$Pointer.release_directory).TrimEnd('\') + '\'
    if (-not $directory.StartsWith($releaseRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'ROLLBACK_RELEASE_OUTSIDE_APP_ROOT' }
    $binary = Join-Path $Pointer.release_directory 'exam-monitor.exe'
    $manifestPath = Join-Path $Pointer.release_directory 'release-manifest.json'
    $expectedConfig = [IO.Path]::GetFullPath((Join-Path $Pointer.release_directory 'exam-monitor.json'))
    if (-not ([IO.Path]::GetFullPath([string]$Pointer.config_path)).Equals($expectedConfig, [StringComparison]::OrdinalIgnoreCase) -or -not (Test-Path -LiteralPath $binary -PathType Leaf) -or -not (Test-Path -LiteralPath $manifestPath -PathType Leaf) -or -not (Test-Path -LiteralPath $Pointer.config_path -PathType Leaf)) { throw 'ROLLBACK_RELEASE_MISSING' }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($manifest.schema_version -ne 1 -or [string]$Pointer.version -ne [string]$manifest.version -or (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant() -ne $manifest.binary.sha256) { throw 'ROLLBACK_RELEASE_INVALID' }
    return [pscustomobject]@{ Pointer = $Pointer; Binary = $binary; Manifest = $manifest }
}

function Write-AtomicJSON {
    param([string]$Path, [object]$Value)
    $temporary = "$Path.$([guid]::NewGuid().ToString('N')).tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $Path -Force
}

function Test-SchemaRange {
    param([object]$Schema, [object]$Manifest)
    foreach ($name in @('core', 'media', 'm3', 'm4')) {
        $value = [int]$Schema.$name
        $range = $Manifest.database_schema.$name
        if ($null -eq $range -or $value -lt [int]$range.minimum -or $value -gt [int]$range.maximum) { return $false }
    }
    return $true
}

function Read-ConfigCheck {
    param([object]$Release)
    $output = @(& $Release.Binary "--config=$($Release.Pointer.config_path)" --check-config)
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) { throw 'ROLLBACK_CONFIG_INCOMPATIBLE' }
    try { $check = (($output | Out-String).Trim() | ConvertFrom-Json) } catch { throw 'ROLLBACK_CONFIG_INCOMPATIBLE' }
    if ($check.status -ne 'ok' -or [string]::IsNullOrWhiteSpace([string]$check.database_path)) { throw 'ROLLBACK_CONFIG_INCOMPATIBLE' }
    return $check
}

function Read-SchemaInfo {
    param([object[]]$Releases, [string]$ActiveConfigPath)
    foreach ($release in $Releases) {
        $savedPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $exitCode = -1
        try {
            $output = @(& $release.Binary "--config=$ActiveConfigPath" --schema-info 2>$null)
            $exitCode = $LASTEXITCODE
        }
        finally { $ErrorActionPreference = $savedPreference }
        if ($exitCode -ne 0) { continue }
        try { $schema = (($output | Out-String).Trim() | ConvertFrom-Json) } catch { continue }
        if ($null -ne $schema.core -and $null -ne $schema.media -and $null -ne $schema.m3 -and $null -ne $schema.m4) { return $schema }
    }
    throw 'ROLLBACK_SCHEMA_READ_FAILED'
}

function Invoke-TaskCommand {
    param([string[]]$Arguments)
    $savedPreference = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & schtasks.exe @Arguments 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $savedPreference }
}

if (-not [IO.Path]::IsPathRooted($AppRoot) -or $HealthTimeoutSeconds -lt 1 -or $HealthTimeoutSeconds -gt 120) { throw 'ROLLBACK_ARGUMENT_INVALID' }
$currentPath = Join-Path $AppRoot 'current.json'
$previousPath = Join-Path $AppRoot 'previous.json'
if (-not (Test-Path -LiteralPath $currentPath -PathType Leaf) -or -not (Test-Path -LiteralPath $previousPath -PathType Leaf)) { throw 'ROLLBACK_PREVIOUS_RELEASE_MISSING' }
$currentPointer = Get-Content -LiteralPath $currentPath -Raw -Encoding UTF8 | ConvertFrom-Json
$previousPointer = Get-Content -LiteralPath $previousPath -Raw -Encoding UTF8 | ConvertFrom-Json
$current = Read-Release -Pointer $currentPointer
$previous = Read-Release -Pointer $previousPointer
$currentCheck = Read-ConfigCheck -Release $current
$previousCheck = Read-ConfigCheck -Release $previous
if (-not ([IO.Path]::GetFullPath([string]$currentCheck.database_path)).Equals([IO.Path]::GetFullPath([string]$previousCheck.database_path), [StringComparison]::OrdinalIgnoreCase)) { throw 'ROLLBACK_DATA_DIRECTORY_MISMATCH' }
$schema = Read-SchemaInfo -Releases @($current, $previous) -ActiveConfigPath $current.Pointer.config_path
if (-not (Test-SchemaRange -Schema $schema -Manifest $previous.Manifest)) { throw 'ROLLBACK_SCHEMA_INCOMPATIBLE' }

$result = [ordered]@{ schema_version = 1; status = 'planned'; from_version = $current.Pointer.version; to_version = $previous.Pointer.version; database_unchanged = $true; down_migration = $false }
if ($PlanOnly) { $result | ConvertTo-Json; return }

if (-not $NoTaskControl) { Stop-ExamMonitorManagedProcesses -AppRoot $AppRoot -TaskName $TaskName -ErrorPrefix 'ROLLBACK' }
try {
    Write-AtomicJSON -Path $currentPath -Value $previous.Pointer
    if (-not $NoTaskControl) {
        $taskExit = Invoke-TaskCommand -Arguments @('/Run','/TN',$TaskName)
        if ($taskExit -ne 0) { throw 'ROLLBACK_TASK_START_FAILED' }
        $configuration = Get-Content -LiteralPath $previous.Pointer.config_path -Raw -Encoding UTF8 | ConvertFrom-Json
        $baseURL = 'http://' + $configuration.server.listen_address
        $deadline = [DateTime]::UtcNow.AddSeconds($HealthTimeoutSeconds)
        $healthy = $false
        $lastHealth = 'no response'
        while ([DateTime]::UtcNow -lt $deadline) {
            try {
                $live = Invoke-RestMethod -Uri "$baseURL/health/live" -Method Get -TimeoutSec 2
                $ready = Invoke-RestMethod -Uri "$baseURL/health/ready" -Method Get -TimeoutSec 2
                if ($live.status -eq 'ok' -and $live.version -eq $previous.Pointer.version -and $ready.status -eq 'writable') { $healthy = $true; break }
                $lastHealth = "live=$(ConvertTo-Json -InputObject $live -Compress) ready=$(ConvertTo-Json -InputObject $ready -Compress)"
            } catch { $lastHealth = $_.Exception.Message }
            if (-not $healthy) { Start-Sleep -Milliseconds 250 }
        }
        if (-not $healthy) { throw "ROLLBACK_HEALTH_CHECK_FAILED:$lastHealth" }
    }
    Write-AtomicJSON -Path $previousPath -Value $current.Pointer
    $result.status = 'rolled_back'
    $result | ConvertTo-Json
}
catch {
    if (-not $NoTaskControl) { Stop-ExamMonitorManagedProcesses -AppRoot $AppRoot -TaskName $TaskName -ErrorPrefix 'ROLLBACK_RECOVERY' }
    Write-AtomicJSON -Path $currentPath -Value $current.Pointer
    if (-not $NoTaskControl) { $null = Invoke-TaskCommand -Arguments @('/Run','/TN',$TaskName) }
    throw
}
