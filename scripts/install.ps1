[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)][string]$BinaryPath,
    [Parameter(Mandatory)][string]$ConfigPath,
    [string]$AppRoot = (Join-Path $env:LOCALAPPDATA 'ExamMonitor'),
    [string]$TaskName = 'ExamMonitor Recorder Core',
    [switch]$PlanOnly,
    [switch]$NoStart,
    [int]$HealthTimeoutSeconds = 45
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'process-control.ps1')

function Write-Utf8Atomic {
    param([string]$Path, [object]$Value)
    $temporary = "$Path.$([guid]::NewGuid().ToString('N')).tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $Path -Force
}

function Invoke-TaskCommand {
    param([string[]]$Arguments)
    $savedPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & schtasks.exe @Arguments 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $savedPreference }
}

foreach ($path in @($BinaryPath, $ConfigPath, $AppRoot)) {
    if (-not [IO.Path]::IsPathRooted($path)) { throw 'INSTALL_ABSOLUTE_PATH_REQUIRED' }
}
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf) -or -not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)) { throw 'INSTALL_INPUT_MISSING' }
if ([string]::IsNullOrWhiteSpace($TaskName) -or $TaskName.IndexOfAny([char[]]"`r`n") -ge 0 -or $HealthTimeoutSeconds -lt 1 -or $HealthTimeoutSeconds -gt 120) { throw 'INSTALL_ARGUMENT_INVALID' }

$manifestPath = Join-Path (Split-Path -Parent $BinaryPath) 'release-manifest.json'
if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { throw 'INSTALL_RELEASE_MANIFEST_MISSING' }
$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
$binaryHash = (Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($manifest.schema_version -ne 1 -or [string]$manifest.version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$' -or $manifest.binary.sha256 -ne $binaryHash -or $manifest.config_schema.minimum -gt 1 -or $manifest.config_schema.maximum -lt 1) { throw 'INSTALL_RELEASE_MANIFEST_INVALID' }
$versionOutput = @(& $BinaryPath --version)
$versionExit = $LASTEXITCODE
try { $versionInfo = (($versionOutput | Out-String).Trim() | ConvertFrom-Json) } catch { throw 'INSTALL_RELEASE_MANIFEST_INVALID' }
if ($versionExit -ne 0 -or $versionInfo.version -ne $manifest.version -or $versionInfo.commit -ne $manifest.commit -or $versionInfo.build_time_utc -ne $manifest.build_time_utc) { throw 'INSTALL_RELEASE_MANIFEST_INVALID' }
$configCheckOutput = @(& $BinaryPath "--config=$ConfigPath" --check-config)
$configCheckExit = $LASTEXITCODE
try { $configCheck = (($configCheckOutput | Out-String).Trim() | ConvertFrom-Json) } catch { throw 'INSTALL_CONFIG_INVALID' }
if ($configCheckExit -ne 0 -or $configCheck.status -ne 'ok' -or [int]$configCheck.schema_version -lt [int]$manifest.config_schema.minimum -or [int]$configCheck.schema_version -gt [int]$manifest.config_schema.maximum) { throw 'INSTALL_CONFIG_INVALID' }
$configHash = (Get-FileHash -LiteralPath $ConfigPath -Algorithm SHA256).Hash.ToLowerInvariant()
$manifestHash = (Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant()

$existingCurrentPath = Join-Path $AppRoot 'current.json'
if (Test-Path -LiteralPath $existingCurrentPath -PathType Leaf) {
    $existingPointer = Get-Content -LiteralPath $existingCurrentPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $managedReleaseRoot = [IO.Path]::GetFullPath((Join-Path $AppRoot 'releases')).TrimEnd('\') + '\'
    $existingRelease = [IO.Path]::GetFullPath([string]$existingPointer.release_directory).TrimEnd('\') + '\'
    if (-not $existingRelease.StartsWith($managedReleaseRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'INSTALL_CURRENT_RELEASE_INVALID' }
    $existingBinary = Join-Path $existingPointer.release_directory 'exam-monitor.exe'
    $expectedExistingConfig = [IO.Path]::GetFullPath((Join-Path $existingPointer.release_directory 'exam-monitor.json'))
    if (-not ([IO.Path]::GetFullPath([string]$existingPointer.config_path)).Equals($expectedExistingConfig, [StringComparison]::OrdinalIgnoreCase) -or -not (Test-Path -LiteralPath $existingBinary -PathType Leaf) -or -not (Test-Path -LiteralPath $existingPointer.config_path -PathType Leaf)) { throw 'INSTALL_CURRENT_RELEASE_INVALID' }
    $existingCheckOutput = @(& $existingBinary "--config=$($existingPointer.config_path)" --check-config)
    $existingCheckExit = $LASTEXITCODE
    try { $existingCheck = (($existingCheckOutput | Out-String).Trim() | ConvertFrom-Json) } catch { throw 'INSTALL_CURRENT_CONFIG_INVALID' }
    if ($existingCheckExit -ne 0 -or $existingCheck.status -ne 'ok') { throw 'INSTALL_CURRENT_CONFIG_INVALID' }
    $existingDatabase = [IO.Path]::GetFullPath([string]$existingCheck.database_path)
    $newDatabase = [IO.Path]::GetFullPath([string]$configCheck.database_path)
    if (-not $existingDatabase.Equals($newDatabase, [StringComparison]::OrdinalIgnoreCase)) { throw 'INSTALL_DATA_DIRECTORY_CHANGE_REQUIRES_RESTORE' }
}

$plan = [ordered]@{ schema_version = 1; status = 'planned'; task_name = $TaskName; app_root = $AppRoot; version = $manifest.version; run_level = 'limited'; trigger = 'logon'; restart_count = 3 }
if ($PlanOnly) { $plan | ConvertTo-Json -Depth 6; return }

$releaseRoot = Join-Path $AppRoot 'releases'
$releaseDirectory = Join-Path $releaseRoot $manifest.version
$releaseRootFull = [IO.Path]::GetFullPath($releaseRoot).TrimEnd('\') + '\'
$releaseDirectoryFull = [IO.Path]::GetFullPath($releaseDirectory).TrimEnd('\') + '\'
if (-not $releaseDirectoryFull.StartsWith($releaseRootFull, [StringComparison]::OrdinalIgnoreCase)) { throw 'INSTALL_RELEASE_PATH_INVALID' }
$staging = "$releaseDirectory.partial-$([guid]::NewGuid().ToString('N'))"
[void](New-Item -ItemType Directory -Path $releaseRoot -Force)
if (-not (Test-Path -LiteralPath $releaseDirectory -PathType Container)) {
    [void](New-Item -ItemType Directory -Path $staging)
    Copy-Item -LiteralPath $BinaryPath -Destination (Join-Path $staging 'exam-monitor.exe')
    Copy-Item -LiteralPath $manifestPath -Destination (Join-Path $staging 'release-manifest.json')
    Copy-Item -LiteralPath $ConfigPath -Destination (Join-Path $staging 'exam-monitor.json')
    if ((Get-FileHash -LiteralPath (Join-Path $staging 'exam-monitor.exe') -Algorithm SHA256).Hash.ToLowerInvariant() -ne $binaryHash) { throw 'INSTALL_STAGED_BINARY_INVALID' }
    Move-Item -LiteralPath $staging -Destination $releaseDirectory
}
else {
    $installedBinary = Join-Path $releaseDirectory 'exam-monitor.exe'
    $installedConfig = Join-Path $releaseDirectory 'exam-monitor.json'
    $installedManifest = Join-Path $releaseDirectory 'release-manifest.json'
    if (-not (Test-Path -LiteralPath $installedBinary -PathType Leaf) -or -not (Test-Path -LiteralPath $installedConfig -PathType Leaf) -or -not (Test-Path -LiteralPath $installedManifest -PathType Leaf)) { throw 'INSTALL_RELEASE_VERSION_CONFLICT' }
    if ((Get-FileHash -LiteralPath $installedBinary -Algorithm SHA256).Hash.ToLowerInvariant() -ne $binaryHash -or (Get-FileHash -LiteralPath $installedConfig -Algorithm SHA256).Hash.ToLowerInvariant() -ne $configHash -or (Get-FileHash -LiteralPath $installedManifest -Algorithm SHA256).Hash.ToLowerInvariant() -ne $manifestHash) { throw 'INSTALL_RELEASE_VERSION_CONFLICT' }
}

$supervisorTarget = Join-Path $AppRoot 'run-supervised.ps1'
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'run-supervised.ps1') -Destination $supervisorTarget -Force
$currentPath = Join-Path $AppRoot 'current.json'
$previousPath = Join-Path $AppRoot 'previous.json'
$hadCurrent = Test-Path -LiteralPath $currentPath -PathType Leaf
$hadPrevious = Test-Path -LiteralPath $previousPath -PathType Leaf
$oldCurrentRaw = if ($hadCurrent) { Get-Content -LiteralPath $currentPath -Raw -Encoding UTF8 } else { $null }
$oldPreviousRaw = if ($hadPrevious) { Get-Content -LiteralPath $previousPath -Raw -Encoding UTF8 } else { $null }
Stop-ExamMonitorManagedProcesses -AppRoot $AppRoot -TaskName $TaskName -ErrorPrefix 'INSTALL'
if (Test-Path -LiteralPath $currentPath -PathType Leaf) {
    $old = Get-Content -LiteralPath $currentPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($old.version -ne $manifest.version) { Write-Utf8Atomic -Path $previousPath -Value $old }
}
$pointer = [ordered]@{ schema_version = 1; version = $manifest.version; release_directory = $releaseDirectory; config_path = (Join-Path $releaseDirectory 'exam-monitor.json'); installed_at_utc = [DateTime]::UtcNow.ToString('o') }
Write-Utf8Atomic -Path $currentPath -Value $pointer

$identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$command = "$([Security.SecurityElement]::Escape($PSHOME + '\powershell.exe'))"
$arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$supervisorTarget`" -AppRoot `"$AppRoot`""
$xml = @"
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Exam Monitor Recorder Core current-user supervisor</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>$([Security.SecurityElement]::Escape($identity))</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>$([Security.SecurityElement]::Escape($identity))</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure><Enabled>true</Enabled></Settings>
  <Actions Context="Author"><Exec><Command>$command</Command><Arguments>$([Security.SecurityElement]::Escape($arguments))</Arguments></Exec></Actions>
</Task>
"@
$taskXML = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor-task-" + [guid]::NewGuid().ToString('N') + '.xml')
try {
    [IO.File]::WriteAllText($taskXML, $xml, [Text.Encoding]::Unicode)
    $taskExit = Invoke-TaskCommand -Arguments @('/Create','/TN',$TaskName,'/XML',$taskXML,'/F')
    if ($taskExit -ne 0) { throw "INSTALL_TASK_REGISTER_FAILED:$taskExit" }
    if (-not $NoStart) {
        $taskExit = Invoke-TaskCommand -Arguments @('/Run','/TN',$TaskName)
        if ($taskExit -ne 0) { throw "INSTALL_TASK_START_FAILED:$taskExit" }
        $baseURL = 'http://' + $configCheck.listen_address
        $deadline = [DateTime]::UtcNow.AddSeconds($HealthTimeoutSeconds)
        $healthy = $false
        $lastHealth = 'no response'
        while ([DateTime]::UtcNow -lt $deadline) {
            try {
                $live = Invoke-RestMethod -Uri "$baseURL/health/live" -Method Get -TimeoutSec 2
                $ready = Invoke-RestMethod -Uri "$baseURL/health/ready" -Method Get -TimeoutSec 2
                if ($live.status -eq 'ok' -and $live.version -eq $manifest.version -and $ready.status -eq 'writable') { $healthy = $true; break }
                $lastHealth = "live=$(ConvertTo-Json -InputObject $live -Compress) ready=$(ConvertTo-Json -InputObject $ready -Compress)"
            } catch { $lastHealth = $_.Exception.Message }
            if (-not $healthy) { Start-Sleep -Milliseconds 250 }
        }
        if (-not $healthy) { throw "INSTALL_HEALTH_CHECK_FAILED:$lastHealth" }
    }
}
catch {
    $installFailure = $_
    Stop-ExamMonitorManagedProcesses -AppRoot $AppRoot -TaskName $TaskName -ErrorPrefix 'INSTALL_ROLLBACK'
    if ($hadCurrent) { [IO.File]::WriteAllText($currentPath, $oldCurrentRaw, (New-Object Text.UTF8Encoding($false))) } else { Remove-Item -LiteralPath $currentPath -Force -ErrorAction SilentlyContinue }
    if ($hadPrevious) { [IO.File]::WriteAllText($previousPath, $oldPreviousRaw, (New-Object Text.UTF8Encoding($false))) } else { Remove-Item -LiteralPath $previousPath -Force -ErrorAction SilentlyContinue }
    if ($hadCurrent -and -not $NoStart) { $null = Invoke-TaskCommand -Arguments @('/Run','/TN',$TaskName) }
    if (-not $hadCurrent) { $null = Invoke-TaskCommand -Arguments @('/Delete','/TN',$TaskName,'/F') }
    throw $installFailure
}
finally { Remove-Item -LiteralPath $taskXML -Force -ErrorAction SilentlyContinue }

$plan.status = 'installed'
$plan | ConvertTo-Json -Depth 6
