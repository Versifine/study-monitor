[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BinaryPath,
    [Parameter(Mandatory)][string]$SourceConfigPath,
    [Parameter(Mandatory)][string]$SourceDataDirectory
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'process-control.ps1')
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ('em m4 ' + [guid]::NewGuid().ToString('N').Substring(0, 8))
$taskName = 'ExamMonitor M4 Smoke ' + [guid]::NewGuid().ToString('N')
$supervisor = $null
$minimumProcess = $null
$rollbackProcess = $null
$preM4Process = $null
$blockedListener = $null
$previousWorktree = $null
$repoRoot = $null
$bodyFailure = $null
$appRoot = $null

function Write-Utf8 {
    param([string]$Path, [object]$Value)
    [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 12), (New-Object Text.UTF8Encoding($false)))
}

function Get-FreeLoopbackPort {
    $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
    try { $listener.Start(); return ([Net.IPEndPoint]$listener.LocalEndpoint).Port } finally { $listener.Stop() }
}

function Wait-Ready {
    param([Diagnostics.Process]$Process, [string]$BaseURL, [int]$Seconds = 8)
    $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($Process.HasExited) { break }
        try { $ready = Invoke-RestMethod -Uri "$BaseURL/health/ready" -Method Get -TimeoutSec 1; if ($ready.status -eq 'writable') { return $ready } } catch { Start-Sleep -Milliseconds 100 }
    }
    throw 'M4_SMOKE_READINESS_TIMEOUT'
}

function Wait-ServiceVersion {
    param([string]$BaseURL, [string]$Version, [int]$Seconds = 15)
    $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        try {
            $live = Invoke-RestMethod -Uri "$BaseURL/health/live" -Method Get -TimeoutSec 2
            $ready = Invoke-RestMethod -Uri "$BaseURL/health/ready" -Method Get -TimeoutSec 2
            if ($live.version -eq $Version -and $ready.status -eq 'writable') { return }
        } catch { }
        Start-Sleep -Milliseconds 100
    }
    throw "M4_SERVICE_VERSION_TIMEOUT:$Version"
}

function Get-ManagedProcesses {
    param([string]$AppRoot)
    $root = [IO.Path]::GetFullPath((Join-Path $AppRoot 'releases')).TrimEnd('\') + '\'
    return @(Get-Process -Name 'exam-monitor' -ErrorAction SilentlyContinue | Where-Object {
        try { ([IO.Path]::GetFullPath([string]$_.Path)).StartsWith($root, [StringComparison]::OrdinalIgnoreCase) } catch { $false }
    })
}

function Wait-SupervisorState {
    param([string]$Path, [scriptblock]$Predicate, [string]$Description)
    $deadline = [DateTime]::UtcNow.AddSeconds(12)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path -PathType Leaf) {
            try { $state = Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json; if (& $Predicate $state) { return $state } } catch { }
        }
        Start-Sleep -Milliseconds 100
    }
    throw "M4_SUPERVISOR_STATE_TIMEOUT:$Description"
}

function Invoke-TaskCommand {
    param([string[]]$Arguments)
    $savedPreference = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & schtasks.exe @Arguments 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $savedPreference }
}

try {
    [void](New-Item -ItemType Directory -Path $temporaryRoot)

    # Minimum mode is exercised as a running process; unavailable ActivityWatch remains isolated.
    $minimum = Get-Content -LiteralPath $SourceConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $minimumPort = Get-FreeLoopbackPort
    $minimumData = Join-Path $temporaryRoot 'minimum-data'
    $minimumInbox = Join-Path $temporaryRoot 'minimum-inbox'
    [void](New-Item -ItemType Directory -Path $minimumInbox)
    $minimum.runtime.mode = 'minimum'
    $minimum.paths.data_directory = $minimumData
    $minimum.server.listen_address = "127.0.0.1:$minimumPort"
    $minimum.media_ingest.inbox_directory = $minimumInbox
    $mediaCollector = @($minimum.collectors | Where-Object { $_.kind -eq 'media' })[0]
    $minimum.collectors = @(
        [ordered]@{
            id = 'minimum.activitywatch'; kind = 'activitywatch'; enabled = $true
            heartbeat_period = '1m'; allowed_lateness = '1m'; offline_after = '5m'
            planned_schedule = [ordered]@{ timezone = 'UTC'; windows = @([ordered]@{ days = @('monday','tuesday','wednesday','thursday','friday','saturday','sunday'); start_local = '00:00'; end_local = '24:00' }) }
            activitywatch = [ordered]@{ base_url = 'http://127.0.0.1:9'; bucket_id = 'unavailable'; poll_interval = '1s'; request_timeout = '100ms'; initial_lookback = '1h'; rescan_window = '1m'; page_size = 10; max_pages_per_poll = 2; max_response_bytes = 65536; clock_error_ms = 1000 }
        },
        $mediaCollector
    )
    $minimumConfig = Join-Path $temporaryRoot 'minimum.json'
    Write-Utf8 -Path $minimumConfig -Value $minimum
    $minimumArguments = '--config="' + $minimumConfig + '" --run-for=5s'
    $minimumProcess = Start-Process -FilePath $BinaryPath -ArgumentList $minimumArguments -WindowStyle Hidden -PassThru
    $minimumURL = "http://127.0.0.1:$minimumPort"
    [void](Wait-Ready -Process $minimumProcess -BaseURL $minimumURL)
    $operations = Invoke-RestMethod -Uri "$minimumURL/api/v1/operations/status" -Method Get -TimeoutSec 2
    if ($operations.schema_version -ne 1 -or -not $operations.disk_level) { throw 'M4_OPERATIONS_STATUS_INVALID' }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$minimumURL/api/v1/events/batch" -Method Post -ContentType 'application/json' -Body '{"schema_version":1,"events":[]}' -TimeoutSec 2 | Out-Null
        throw 'M4_MINIMUM_GENERIC_INPUT_ENABLED'
    } catch { if ($_.Exception.Response.StatusCode.value__ -ne 404) { throw } }
    if (-not $minimumProcess.WaitForExit(10000) -or $minimumProcess.ExitCode -ne 0) { throw 'M4_MINIMUM_PROCESS_FAILED' }
    $minimumProcess = $null

    # A complete backup is verified, restored into a new directory, and never switches current data.
    $backupDestination = Join-Path $temporaryRoot 'backups'
    $backupJSON = ((& (Join-Path $PSScriptRoot 'backup.ps1') -BinaryPath $BinaryPath -ConfigPath $SourceConfigPath -DestinationDirectory $backupDestination -Type full) | Out-String).Trim()
    $backup = $backupJSON | ConvertFrom-Json
    if ($backup.status -ne 'complete' -or $backup.type -ne 'full') { throw "M4_BACKUP_FAILED:$backupJSON" }
    $restoreTarget = Join-Path $temporaryRoot 'restored-data'
    $restoreJSON = ((& (Join-Path $PSScriptRoot 'restore.ps1') -BackupDirectory $backup.backup_directory -VerifierBinaryPath $BinaryPath -TargetDirectory $restoreTarget) | Out-String).Trim()
    $restore = $restoreJSON | ConvertFrom-Json
    if ($restore.status -ne 'verified' -or $restore.switched -or -not (Test-Path -LiteralPath (Join-Path $restoreTarget 'exam-monitor.db') -PathType Leaf)) { throw "M4_RESTORE_FAILED:$restoreJSON" }
    $sourceMedia = @(Get-ChildItem -LiteralPath (Join-Path $SourceDataDirectory 'media\accepted') -Filter '*.media' -File)
    foreach ($media in $sourceMedia) {
        $restoredMedia = Join-Path $restoreTarget ('media\accepted\' + $media.Name)
        if (-not (Test-Path -LiteralPath $restoredMedia -PathType Leaf) -or (Get-FileHash -LiteralPath $restoredMedia -Algorithm SHA256).Hash -ne (Get-FileHash -LiteralPath $media.FullName -Algorithm SHA256).Hash) { throw 'M4_RESTORED_MEDIA_INVALID' }
    }

    # Interrupted backup cannot replace the last verified marker.
    $markerPath = Join-Path $SourceDataDirectory 'backup\latest-full.json'
    $markerBefore = Get-Content -LiteralPath $markerPath -Raw -Encoding UTF8
    try { & (Join-Path $PSScriptRoot 'backup.ps1') -BinaryPath $BinaryPath -ConfigPath $SourceConfigPath -DestinationDirectory $backupDestination -Type full -InjectFailureAfterFiles 1 | Out-Null; throw 'M4_BACKUP_INTERRUPTION_NOT_INJECTED' } catch { if ($_.Exception.Message -notmatch 'BACKUP_INJECTED_INTERRUPTION') { throw } }
    if ((Get-Content -LiteralPath $markerPath -Raw -Encoding UTF8) -ne $markerBefore -or @(Get-ChildItem -LiteralPath $backupDestination -Filter '*.partial' -Force).Count -ne 0) { throw 'M4_INTERRUPTED_BACKUP_REPLACED_LAST_GOOD' }

    # Interrupted restore leaves neither a switched nor a partial target.
    $interruptedTarget = Join-Path $temporaryRoot 'interrupted-restore'
    try { & (Join-Path $PSScriptRoot 'restore.ps1') -BackupDirectory $backup.backup_directory -VerifierBinaryPath $BinaryPath -TargetDirectory $interruptedTarget -InjectFailureAfterFiles 1 | Out-Null; throw 'M4_RESTORE_INTERRUPTION_NOT_INJECTED' } catch { if ($_.Exception.Message -notmatch 'RESTORE_INJECTED_INTERRUPTION') { throw } }
    if (Test-Path -LiteralPath $interruptedTarget) { throw 'M4_INTERRUPTED_RESTORE_SWITCHED_TARGET' }

    # Corrupt backup is rejected before a target is created.
    $corruptBackup = Join-Path $temporaryRoot 'corrupt-backup'
    [void](New-Item -ItemType Directory -Path $corruptBackup)
    Copy-Item -Path (Join-Path $backup.backup_directory '*') -Destination $corruptBackup -Recurse
    $corruptDatabase = Join-Path $corruptBackup 'database\exam-monitor.db'
    $stream = [IO.File]::Open($corruptDatabase, [IO.FileMode]::Open, [IO.FileAccess]::Write)
    try { $stream.SetLength(1) } finally { $stream.Dispose() }
    $corruptTarget = Join-Path $temporaryRoot 'corrupt-target'
    try { & (Join-Path $PSScriptRoot 'restore.ps1') -BackupDirectory $corruptBackup -VerifierBinaryPath $BinaryPath -TargetDirectory $corruptTarget | Out-Null; throw 'M4_CORRUPT_BACKUP_ACCEPTED' } catch { if ($_.Exception.Message -notmatch 'RESTORE_BACKUP_CORRUPT') { throw } }
    if (Test-Path -LiteralPath $corruptTarget) { throw 'M4_CORRUPT_BACKUP_CREATED_TARGET' }

    # A manifest cannot omit database-authoritative media or escape the backup root.
    $omittedBackup = Join-Path $temporaryRoot 'omitted-media-backup'
    [void](New-Item -ItemType Directory -Path $omittedBackup)
    Copy-Item -Path (Join-Path $backup.backup_directory '*') -Destination $omittedBackup -Recurse
    $omittedManifestPath = Join-Path $omittedBackup 'manifest.json'
    $omittedManifest = Get-Content -LiteralPath $omittedManifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $mediaEntries = @($omittedManifest.files | Where-Object { $_.kind -eq 'media' })
    if ($mediaEntries.Count -gt 0) {
        $omittedManifest.files = @($omittedManifest.files | Where-Object { $_ -ne $mediaEntries[0] })
        Write-Utf8 -Path $omittedManifestPath -Value $omittedManifest
        $omittedTarget = Join-Path $temporaryRoot 'omitted-media-target'
        try { & (Join-Path $PSScriptRoot 'restore.ps1') -BackupDirectory $omittedBackup -VerifierBinaryPath $BinaryPath -TargetDirectory $omittedTarget | Out-Null; throw 'M4_OMITTED_MEDIA_ACCEPTED' } catch { if ($_.Exception.Message -notmatch 'RESTORE_MEDIA_COVERAGE') { throw } }
        if (Test-Path -LiteralPath $omittedTarget) { throw 'M4_OMITTED_MEDIA_CREATED_TARGET' }
    }
    $traversalBackup = Join-Path $temporaryRoot 'traversal-backup'
    [void](New-Item -ItemType Directory -Path $traversalBackup)
    Copy-Item -Path (Join-Path $backup.backup_directory '*') -Destination $traversalBackup -Recurse
    $traversalManifestPath = Join-Path $traversalBackup 'manifest.json'
    $traversalManifest = Get-Content -LiteralPath $traversalManifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $metadataEntry = @($traversalManifest.files | Where-Object { $_.kind -eq 'metadata' })[0]
    $metadataEntry.backup_path = 'metadata\..\..\outside.txt'
    Write-Utf8 -Path $traversalManifestPath -Value $traversalManifest
    try { & (Join-Path $PSScriptRoot 'restore.ps1') -BackupDirectory $traversalBackup -VerifierBinaryPath $BinaryPath -TargetDirectory (Join-Path $temporaryRoot 'traversal-target') | Out-Null; throw 'M4_TRAVERSAL_MANIFEST_ACCEPTED' } catch { if ($_.Exception.Message -notmatch 'RESTORE_MANIFEST_(PATH_)?INVALID') { throw } }

    # Build the actual M3 checkpoint, prove pre-M4 schema inspection returns m4=0,
    # then install M3 before the M4 candidate so rollback exercises the real stable binary.
    $repoRoot = ((& git -C $PSScriptRoot rev-parse --show-toplevel) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $repoRoot) { throw 'M4_REPOSITORY_ROOT_UNAVAILABLE' }
    $previousWorktree = Join-Path $temporaryRoot 'previous m3 worktree'
    & git -C $repoRoot worktree add --detach $previousWorktree 89ed656 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'M4_PREVIOUS_WORKTREE_FAILED' }
    $m3Build = Join-Path $temporaryRoot 'm3 build'
    $m3Binary = ((& (Join-Path $previousWorktree 'scripts\build.ps1') -OutputDirectory $m3Build) | Select-Object -Last 1)
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $m3Binary -PathType Leaf)) { throw 'M4_PREVIOUS_BUILD_FAILED' }
    $m3Version = ((& $m3Binary --version) | Out-String).Trim() | ConvertFrom-Json
    $m3Manifest = [ordered]@{
        schema_version = 1; version = $m3Version.version; commit = $m3Version.commit; build_time_utc = $m3Version.build_time_utc; platform = 'windows/amd64'
        binary = [ordered]@{ name = 'exam-monitor.exe'; sha256 = (Get-FileHash -LiteralPath $m3Binary -Algorithm SHA256).Hash.ToLowerInvariant() }
        config_schema = [ordered]@{ minimum = 1; maximum = 1 }
        database_schema = [ordered]@{ core = [ordered]@{ minimum = 1; maximum = 1 }; media = [ordered]@{ minimum = 2; maximum = 2 }; m3 = [ordered]@{ minimum = 1; maximum = 1 }; m4 = [ordered]@{ minimum = 0; maximum = 1 } }
    }
    Write-Utf8 -Path (Join-Path $m3Build 'release-manifest.json') -Value $m3Manifest
    $m3ConfigValue = Get-Content -LiteralPath $SourceConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
    [void]$m3ConfigValue.PSObject.Properties.Remove('operations')
    [void]$m3ConfigValue.PSObject.Properties.Remove('retention')
    foreach ($name in @('warning_free_bytes','critical_free_bytes','database_reserve_bytes')) { [void]$m3ConfigValue.storage.PSObject.Properties.Remove($name) }
    foreach ($name in @('file_enabled','max_file_bytes','max_files')) { [void]$m3ConfigValue.logging.PSObject.Properties.Remove($name) }
    $m3Config = Join-Path $temporaryRoot 'm3 config.json'
    Write-Utf8 -Path $m3Config -Value $m3ConfigValue
    & $m3Binary "--config=$m3Config" --check-config | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'M4_PREVIOUS_CONFIG_INVALID' }
    $preM4ConfigValue = Get-Content -LiteralPath $m3Config -Raw -Encoding UTF8 | ConvertFrom-Json
    $preM4ConfigValue.paths.data_directory = Join-Path $temporaryRoot 'pure m3 data'
    $preM4ConfigValue.server.listen_address = '127.0.0.1:' + (Get-FreeLoopbackPort)
    $preM4Config = Join-Path $temporaryRoot 'pure m3 config.json'
    Write-Utf8 -Path $preM4Config -Value $preM4ConfigValue
    $preM4Arguments = '--config="' + $preM4Config + '" --run-for=1s'
    $preM4Process = Start-Process -FilePath $m3Binary -ArgumentList $preM4Arguments -WindowStyle Hidden -PassThru
    if (-not $preM4Process.WaitForExit(8000) -or $preM4Process.ExitCode -ne 0) { throw 'M4_PREVIOUS_DATABASE_CREATE_FAILED' }
    $preM4Process = $null
    $preM4Schema = ((& $BinaryPath "--config=$preM4Config" --schema-info) | Out-String).Trim() | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0 -or $preM4Schema.m4 -ne 0) { throw 'M4_PREVIOUS_SCHEMA_ZERO_NOT_SUPPORTED' }

    # Register a current-user task without starting it, then prove bounded child recovery.
    $appRoot = Join-Path $temporaryRoot 'installed'
    try {
        & (Join-Path $PSScriptRoot 'install.ps1') -BinaryPath $m3Binary -ConfigPath $m3Config -AppRoot $appRoot -TaskName $taskName -HealthTimeoutSeconds 60 | Out-Null
    }
    catch { throw "M4_M3_INSTALL_FAILED:$($_.Exception.Message)" }
    if ((Invoke-TaskCommand -Arguments @('/Query','/TN',$taskName)) -ne 0) { throw 'M4_TASK_NOT_REGISTERED' }
    $serviceURL = 'http://' + $m3ConfigValue.server.listen_address
    Wait-ServiceVersion -BaseURL $serviceURL -Version $m3Version.version
    $candidateVersion = ((& $BinaryPath --version) | Out-String).Trim() | ConvertFrom-Json
    try {
        & (Join-Path $PSScriptRoot 'install.ps1') -BinaryPath $BinaryPath -ConfigPath $SourceConfigPath -AppRoot $appRoot -TaskName $taskName -HealthTimeoutSeconds 60 | Out-Null
    }
    catch { throw "M4_CANDIDATE_INSTALL_FAILED:$($_.Exception.Message)" }
    Wait-ServiceVersion -BaseURL $serviceURL -Version $candidateVersion.version

    # A task start acknowledgement is not an install success: force a health failure,
    # then prove the pointer and running service return to the previous candidate.
    $healthBuild = Join-Path $temporaryRoot 'health failure build'
    $healthBinary = ((& (Join-Path $PSScriptRoot 'build.ps1') -OutputDirectory $healthBuild -Version '0.5.1-m4-health') | Select-Object -Last 1)
    $healthConfigValue = Get-Content -LiteralPath $SourceConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $otherDataConfigValue = Get-Content -LiteralPath $SourceConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $otherDataConfigValue.paths.data_directory = Join-Path $temporaryRoot 'unexpected other data'
    $otherDataConfig = Join-Path $temporaryRoot 'other data config.json'
    Write-Utf8 -Path $otherDataConfig -Value $otherDataConfigValue
    try { & (Join-Path $PSScriptRoot 'install.ps1') -BinaryPath $healthBinary -ConfigPath $otherDataConfig -AppRoot $appRoot -TaskName $taskName -PlanOnly | Out-Null; throw 'M4_INSTALL_DATA_ROOT_CHANGE_ACCEPTED' } catch { if ($_.Exception.Message -notmatch 'INSTALL_DATA_DIRECTORY_CHANGE_REQUIRES_RESTORE') { throw } }
    $blockedListener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
    $blockedListener.Start()
    $blockedPort = ([Net.IPEndPoint]$blockedListener.LocalEndpoint).Port
    $healthConfigValue.server.listen_address = "127.0.0.1:$blockedPort"
    $healthConfig = Join-Path $temporaryRoot 'health failure config.json'
    Write-Utf8 -Path $healthConfig -Value $healthConfigValue
    $healthFailureObserved = $false
    try {
        & (Join-Path $PSScriptRoot 'install.ps1') -BinaryPath $healthBinary -ConfigPath $healthConfig -AppRoot $appRoot -TaskName $taskName -HealthTimeoutSeconds 3 | Out-Null
    }
    catch {
        if ($_.Exception.Message -notmatch 'INSTALL_HEALTH_CHECK_FAILED') { throw }
        $healthFailureObserved = $true
    }
    finally { $blockedListener.Stop(); $blockedListener = $null }
    if (-not $healthFailureObserved) { throw 'M4_INSTALL_HEALTH_FAILURE_ACCEPTED' }
    Wait-ServiceVersion -BaseURL $serviceURL -Version $candidateVersion.version
    $currentAfterFailedInstall = Get-Content -LiteralPath (Join-Path $appRoot 'current.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($currentAfterFailedInstall.version -ne $candidateVersion.version) { throw 'M4_INSTALL_HEALTH_FAILURE_CHANGED_POINTER' }
    $managedAfterUpgrade = @(Get-ManagedProcesses -AppRoot $appRoot)
    if ($managedAfterUpgrade.Count -ne 1 -or $managedAfterUpgrade[0].Path -notlike "*$($candidateVersion.version)*") { throw 'M4_INSTALL_UPGRADE_LEFT_OLD_CHILD' }
    Stop-ExamMonitorManagedProcesses -AppRoot $appRoot -TaskName $taskName -ErrorPrefix 'M4_SMOKE_PREPARE'
    if (@(Get-ManagedProcesses -AppRoot $appRoot).Count -ne 0) { throw 'M4_TASK_END_LEFT_CHILD' }
    $supervisorState = Join-Path $appRoot 'state\supervisor-state.json'
    $poisonCrashes = @(1..5 | ForEach-Object { [DateTime]::UtcNow.AddSeconds(-$_).ToString('o') })
    Write-Utf8 -Path $supervisorState -Value ([ordered]@{ schema_version = 1; status = 'degraded'; release_version = 'different-release'; crash_times_utc = $poisonCrashes; updated_at_utc = [DateTime]::UtcNow.ToString('o') })
    $supervisorScript = Join-Path $appRoot 'run-supervised.ps1'
    $supervisorArguments = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $supervisorScript + '" -AppRoot "' + $appRoot + '" -MaxRestarts 2 -CrashWindowSeconds 60 -InitialBackoffSeconds 1 -MaximumBackoffSeconds 1'
    $supervisor = Start-Process -FilePath 'powershell.exe' -ArgumentList $supervisorArguments -WindowStyle Hidden -PassThru
    $firstState = Wait-SupervisorState -Path $supervisorState -Description 'first child' -Predicate { param($state) $state.status -eq 'running' -and $state.child_pid -gt 0 }
    Stop-Process -Id $firstState.child_pid -Force
    $secondState = Wait-SupervisorState -Path $supervisorState -Description 'restarted child' -Predicate { param($state) $state.status -eq 'running' -and $state.child_pid -gt 0 -and $state.child_pid -ne $firstState.child_pid }
    Stop-Process -Id $secondState.child_pid -Force
    if (-not $supervisor.WaitForExit(12000) -or $supervisor.ExitCode -ne 3) { throw 'M4_SUPERVISOR_CRASH_LOOP_NOT_BOUNDED' }
    $degraded = Get-Content -LiteralPath $supervisorState -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($degraded.status -ne 'degraded' -or $degraded.error_code -ne 'SUPERVISOR_CRASH_LOOP') { throw 'M4_SUPERVISOR_DEGRADED_FACT_MISSING' }
    $supervisor = $null

    # Reject an incompatible M3 manifest while the current pointer still selects the M4 binary.
    $previousBeforeRollback = Get-Content -LiteralPath (Join-Path $appRoot 'previous.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    $previousManifestPath = Join-Path $previousBeforeRollback.release_directory 'release-manifest.json'
    $previousManifestRaw = Get-Content -LiteralPath $previousManifestPath -Raw -Encoding UTF8
    $incompatible = $previousManifestRaw | ConvertFrom-Json
    $incompatible.database_schema.m4.maximum = 0
    Write-Utf8 -Path $previousManifestPath -Value $incompatible
    try { & (Join-Path $PSScriptRoot 'rollback.ps1') -AppRoot $appRoot -TaskName $taskName -PlanOnly | Out-Null; throw 'M4_INCOMPATIBLE_ROLLBACK_ACCEPTED' } catch { if ($_.Exception.Message -notmatch 'ROLLBACK_SCHEMA_INCOMPATIBLE') { throw } }
    [IO.File]::WriteAllText($previousManifestPath, $previousManifestRaw, (New-Object Text.UTF8Encoding($false)))

    # Rollback uses the real scheduled task, verifies the selected M3 version, and never down-migrates the database.
    $schemaBeforeRollback = ((& $BinaryPath "--config=$SourceConfigPath" --schema-info) | Out-String).Trim() | ConvertFrom-Json
    $rollbackJSON = ((& (Join-Path $PSScriptRoot 'rollback.ps1') -AppRoot $appRoot -TaskName $taskName) | Out-String).Trim()
    $rollback = $rollbackJSON | ConvertFrom-Json
    if ($rollback.status -ne 'rolled_back' -or -not $rollback.database_unchanged -or $rollback.down_migration) { throw "M4_ROLLBACK_FAILED:$rollbackJSON" }
    $selected = Get-Content -LiteralPath (Join-Path $appRoot 'current.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    $selectedConfig = Get-Content -LiteralPath $selected.config_path -Raw -Encoding UTF8 | ConvertFrom-Json
    $selectedLive = Invoke-RestMethod -Uri ('http://' + $selectedConfig.server.listen_address + '/health/live') -Method Get -TimeoutSec 2
    if ($selected.version -ne $m3Version.version -or $selectedLive.version -ne $m3Version.version) { throw 'M4_PREVIOUS_STABLE_RELEASE_FAILED' }
    $schemaAfterRollback = ((& $BinaryPath "--config=$SourceConfigPath" --schema-info) | Out-String).Trim() | ConvertFrom-Json
    if ($schemaBeforeRollback.m4 -ne 1 -or $schemaAfterRollback.m4 -ne 1) { throw 'M4_ROLLBACK_DOWN_MIGRATED_DATABASE' }
    $null = Invoke-TaskCommand -Arguments @('/End','/TN',$taskName)

    & (Join-Path $PSScriptRoot 'uninstall.ps1') -AppRoot $appRoot -TaskName $taskName | Out-Null
    if ((Invoke-TaskCommand -Arguments @('/Query','/TN',$taskName)) -eq 0 -or -not (Test-Path -LiteralPath $appRoot -PathType Container) -or @(Get-ManagedProcesses -AppRoot $appRoot).Count -ne 0) { throw 'M4_UNINSTALL_REMOVED_DATA_OR_LEFT_TASK' }

    Write-Output 'M4 smoke passed: Minimum, full backup/new-directory restore, bounded automatic recovery, compatible previous-release rollback, corrupt/interrupted operation rejection, and current-user Task Scheduler lifecycle'
}
catch {
    $bodyFailure = $_
    throw
}
finally {
    $cleanupFailure = $null
    try { if ($null -ne $blockedListener) { $blockedListener.Stop() } } catch { $cleanupFailure = $_ }
    try { foreach ($process in @($supervisor, $minimumProcess, $rollbackProcess, $preM4Process)) { if ($null -ne $process -and -not $process.HasExited) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue } } } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
    try { if ($appRoot) { Stop-ExamMonitorManagedProcesses -AppRoot $appRoot -TaskName $taskName -ErrorPrefix 'M4_SMOKE_CLEANUP' } } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
    try { $null = Invoke-TaskCommand -Arguments @('/Delete','/TN',$taskName,'/F') } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
    try { if ($previousWorktree -and $repoRoot -and (Test-Path -LiteralPath $previousWorktree -PathType Container)) { & git -C $repoRoot worktree remove --force $previousWorktree | Out-Null } } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
    try {
        for ($attempt = 1; $attempt -le 5 -and (Test-Path -LiteralPath $temporaryRoot); $attempt++) {
            try { Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction Stop } catch { if ($attempt -eq 5) { throw }; Start-Sleep -Milliseconds 200 }
        }
    } catch { if ($null -eq $cleanupFailure) { $cleanupFailure = $_ } }
    if ($null -ne $cleanupFailure) {
        if ($null -ne $bodyFailure) { Write-Warning "M4 smoke cleanup also failed: $($cleanupFailure.Exception.Message)" } else { throw $cleanupFailure }
    }
}
