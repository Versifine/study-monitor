[CmdletBinding()]
param(
    [ValidateSet('Initialize', 'InstallTasks', 'RemoveTasks', 'Sample', 'Backup', 'Daily', 'Record', 'Finalize')]
    [string]$Action = 'Sample',
    [string]$CertificationDirectory,
    [string]$BinaryPath,
    [string]$ConfigPath,
    [string]$ReleaseManifestPath,
    [string]$PreviousReleaseDirectory,
    [string]$BackupDirectory,
    [string]$ProfilePath,
    [string]$Date,
    [ValidateSet('network_disconnect', 'process_termination', 'system_reboot', 'write_interruption', 'duplicate_submission', 'corrupt_media', 'low_disk', 'cloud_unavailable', 'clock_offset', 'backup_restore', 'rollback', 'manual_intervention', 'notification')]
    [string]$RecordKind,
    [ValidateSet('planned', 'started', 'passed', 'failed', 'observed')]
    [string]$RecordStatus,
    [string]$RecordDetail,
    [Nullable[double]]$RecordDurationSeconds,
    [ValidateSet('P0', 'P1')]
    [string]$RecordSeverity
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$sourceRepoRoot = Split-Path -Parent $PSScriptRoot
$utf8NoBom = New-Object Text.UTF8Encoding($false)
$requiredFFprobeVersion = 'N-117599-ge1d1ba4cbc-20241017'
$requiredFFmpegVersion = 'N-117599-ge1d1ba4cbc-20241017'
$requiredExerciseKinds = @(
    'network_disconnect', 'process_termination', 'system_reboot', 'write_interruption',
    'duplicate_submission', 'corrupt_media', 'low_disk', 'cloud_unavailable',
    'clock_offset', 'backup_restore', 'rollback'
)

function Assert-AbsolutePath {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code)
    if (-not [IO.Path]::IsPathRooted($Path)) { throw $Code }
    return [IO.Path]::GetFullPath($Path)
}

function Assert-RegularFile {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code)
    $resolved = Assert-AbsolutePath -Path $Path -Code $Code
    $item = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw $Code }
    return $resolved
}

function Assert-RegularDirectory {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code, [switch]$Create)
    $resolved = Assert-AbsolutePath -Path $Path -Code $Code
    if ($Create -and -not (Test-Path -LiteralPath $resolved)) { [void](New-Item -ItemType Directory -Path $resolved) }
    $item = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw $Code }
    return $resolved.TrimEnd('\')
}

function Test-PathContained {
    param([Parameter(Mandatory)][string]$Root, [Parameter(Mandatory)][string]$Candidate)
    $rootPrefix = [IO.Path]::GetFullPath($Root).TrimEnd('\') + '\'
    $candidatePath = [IO.Path]::GetFullPath($Candidate).TrimEnd('\') + '\'
    return $candidatePath.StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)
}

function Get-SHA256 {
    param([Parameter(Mandatory)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Write-JsonAtomic {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)]$Value)
    $parent = Split-Path -Parent $Path
    [void](New-Item -ItemType Directory -Path $parent -Force)
    $temporary = "$Path.$([guid]::NewGuid().ToString('N')).tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 32), $utf8NoBom)
    Move-Item -LiteralPath $temporary -Destination $Path -Force
}

function Append-JsonLine {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)]$Value)
    $parent = Split-Path -Parent $Path
    [void](New-Item -ItemType Directory -Path $parent -Force)
    [IO.File]::AppendAllText($Path, (($Value | ConvertTo-Json -Depth 32 -Compress) + [Environment]::NewLine), $utf8NoBom)
}

function Read-JsonFile {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code)
    try { return (Get-Content -Raw -Encoding UTF8 -LiteralPath $Path | ConvertFrom-Json) }
    catch { throw "$Code`:$($_.Exception.Message)" }
}

function Invoke-BinaryJson {
    param([Parameter(Mandatory)][string]$Executable, [Parameter(Mandatory)][string[]]$Arguments, [Parameter(Mandatory)][string]$Code)
    $output = @(& $Executable @Arguments)
    if ($LASTEXITCODE -ne 0) { throw "$Code`:$LASTEXITCODE" }
    try { return (($output | Out-String).Trim() | ConvertFrom-Json) }
    catch { throw "$Code`:INVALID_JSON" }
}

function Invoke-LocalJson {
    param([Parameter(Mandatory)][string]$Uri, [Parameter(Mandatory)][string]$Code)
    try { return Invoke-RestMethod -Uri $Uri -Method Get -TimeoutSec 8 -UseBasicParsing }
    catch { throw "$Code`:$($_.Exception.Message)" }
}

function Invoke-LocalJsonOrUnavailable {
    param([Parameter(Mandatory)][string]$Uri)
    try { return Invoke-RestMethod -Uri $Uri -Method Get -TimeoutSec 8 -UseBasicParsing }
    catch { return [ordered]@{ status = 'unavailable'; error_code = 'M6_SAMPLE_HTTP_UNAVAILABLE'; detail = $_.Exception.Message } }
}

function Get-TreeMeasurement {
    param([Parameter(Mandatory)][string]$Path, [string]$Filter = '*')
    if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return [ordered]@{ files = 0; bytes = 0 } }
    $files = @(Get-ChildItem -LiteralPath $Path -File -Force -Recurse -Filter $Filter -ErrorAction Stop)
    $bytes = [int64]0
    foreach ($file in $files) { $bytes += [int64]$file.Length }
    return [ordered]@{ files = $files.Count; bytes = $bytes }
}

function Get-DependencyTreeHash {
    param([Parameter(Mandatory)][string]$Root)
    $rows = New-Object Collections.Generic.List[string]
    foreach ($file in @(Get-ChildItem -LiteralPath $Root -File -Force -Recurse | Sort-Object FullName)) {
        $relative = $file.FullName.Substring($Root.Length).TrimStart('\').Replace('\', '/')
        $rows.Add("$relative`t$($file.Length)`t$(Get-SHA256 -Path $file.FullName)")
    }
    $temporary = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor-m6-tree-" + [guid]::NewGuid().ToString('N') + '.txt')
    try {
        [IO.File]::WriteAllLines($temporary, $rows, $utf8NoBom)
        return Get-SHA256 -Path $temporary
    }
    finally { Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue }
}

function Convert-IsoLocalToUtc {
    param([Parameter(Mandatory)][string]$LocalText, [Parameter(Mandatory)][string]$Offset)
    return [DateTimeOffset]::ParseExact("$LocalText$Offset", 'yyyy-MM-ddTHH:mmzzz', [Globalization.CultureInfo]::InvariantCulture).UtcDateTime
}

function Convert-LocalClockToMinutes {
    param([Parameter(Mandatory)][string]$Value)
    if ($Value -eq '24:00') { return 1440 }
    try { $parsed = [TimeSpan]::ParseExact($Value, 'hh\:mm', [Globalization.CultureInfo]::InvariantCulture) }
    catch { throw 'M6_LOCAL_CLOCK_INVALID' }
    return [int]$parsed.TotalMinutes
}

function Read-SealedManifest {
    param([Parameter(Mandatory)][string]$Root)
    $root = Assert-RegularDirectory -Path $Root -Code 'M6_CERTIFICATION_DIRECTORY_INVALID'
    $manifestPath = Join-Path $root 'freeze-manifest.json'
    $sealPath = Join-Path $root 'freeze-manifest.sha256'
    $manifestPath = Assert-RegularFile -Path $manifestPath -Code 'M6_FREEZE_MANIFEST_MISSING'
    $sealPath = Assert-RegularFile -Path $sealPath -Code 'M6_FREEZE_SEAL_MISSING'
    $expected = (Get-Content -Raw -Encoding ASCII -LiteralPath $sealPath).Trim().ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$' -or (Get-SHA256 -Path $manifestPath) -ne $expected) { throw 'M6_FREEZE_MANIFEST_CHANGED' }
    $manifest = Read-JsonFile -Path $manifestPath -Code 'M6_FREEZE_MANIFEST_INVALID'
    if ($manifest.schema_version -ne 1 -or $manifest.required_consecutive_days -ne 14) { throw 'M6_FREEZE_MANIFEST_INVALID' }
    return $manifest
}

function Assert-FrozenInputs {
    param([Parameter(Mandatory)]$Manifest)
    $checks = @(
        @($Manifest.candidate.binary_path, $Manifest.candidate.binary_sha256, 'M6_CANDIDATE_BINARY_CHANGED'),
        @($Manifest.candidate.release_manifest_path, $Manifest.candidate.release_manifest_sha256, 'M6_RELEASE_MANIFEST_CHANGED'),
        @($Manifest.configuration.path, $Manifest.configuration.sha256, 'M6_CONFIG_CHANGED'),
        @($Manifest.tool.path, $Manifest.tool.sha256, 'M6_CERTIFICATION_TOOL_CHANGED'),
        @($Manifest.media_publisher.tool_path, $Manifest.media_publisher.tool_sha256, 'M6_MEDIA_PUBLISHER_TOOL_CHANGED')
    )
    foreach ($check in $checks) {
        if (-not (Test-Path -LiteralPath $check[0] -PathType Leaf) -or (Get-SHA256 -Path $check[0]) -ne $check[1]) { throw $check[2] }
    }
    $pointer = Read-JsonFile -Path (Join-Path $Manifest.paths.app_root 'current.json') -Code 'M6_CURRENT_POINTER_INVALID'
    if ([IO.Path]::GetFullPath([string]$pointer.release_directory) -ne [IO.Path]::GetFullPath([string]$Manifest.candidate.release_directory) -or [string]$pointer.version -ne [string]$Manifest.candidate.version) {
        throw 'M6_ACTIVE_RELEASE_CHANGED'
    }
}

function Assert-FrozenRepository {
    param([Parameter(Mandatory)]$Manifest)
    $root = [string]$Manifest.paths.repository_root
    $head = ((& git -C $root rev-parse HEAD) | Out-String).Trim()
    $status = @(& git -C $root status --porcelain=v1 --untracked-files=all)
    if ($LASTEXITCODE -ne 0 -or $head -ne [string]$Manifest.candidate.commit -or $status.Count -ne 0) { throw 'M6_REPOSITORY_CHANGED' }
    foreach ($dependency in @($Manifest.dependencies)) {
        $path = Join-Path $root ([string]$dependency.path).Replace('/', '\')
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-SHA256 -Path $path) -ne [string]$dependency.sha256) { throw 'M6_DEPENDENCY_CHANGED' }
    }
    if ((Get-DependencyTreeHash -Root (Join-Path $root 'vendor')) -ne [string]$Manifest.vendor_tree_sha256) { throw 'M6_DEPENDENCY_CHANGED' }
}

function Write-Violation {
    param([Parameter(Mandatory)]$Manifest, [Parameter(Mandatory)][string]$Code, [Parameter(Mandatory)][string]$Detail)
    $value = [ordered]@{ schema_version = 1; occurred_at_utc = [DateTime]::UtcNow.ToString('o'); error_code = $Code; detail = $Detail; restart_required = $true }
    Append-JsonLine -Path (Join-Path $Manifest.paths.certification_directory 'violations.jsonl') -Value $value
}

function Read-CertificationRecords {
    param([Parameter(Mandatory)]$Manifest)
    $records = @()
    $path = Join-Path $Manifest.paths.certification_directory 'records\events.jsonl'
    if (Test-Path -LiteralPath $path -PathType Leaf) {
        foreach ($line in Get-Content -Encoding UTF8 -LiteralPath $path) { if ($line.Trim()) { $records += ($line | ConvertFrom-Json) } }
    }
    return $records
}

function Read-MediaPublisherRuns {
    param([Parameter(Mandatory)]$Manifest)
    $runs = @()
    $directory = Join-Path $Manifest.paths.certification_directory 'media-publisher'
    foreach ($file in @(Get-ChildItem -LiteralPath $directory -File -Filter '*.json' -ErrorAction SilentlyContinue)) {
        $runs += Read-JsonFile -Path $file.FullName -Code 'M6_MEDIA_PUBLISHER_RECORD_INVALID'
    }
    return $runs
}

function Get-CoverageSegments {
    param([Parameter(Mandatory)][datetime]$Start, [Parameter(Mandatory)][datetime]$End, [Parameter(Mandatory)]$Excluded)
    $segments = @([ordered]@{ start = $Start; end = $End })
    foreach ($window in @($Excluded)) {
        $windowStart = [DateTime]::Parse([string]$window.start_utc).ToUniversalTime()
        $windowEnd = [DateTime]::Parse([string]$window.end_utc).ToUniversalTime()
        $next = @()
        foreach ($segment in $segments) {
            if ($windowEnd -le $segment.start -or $windowStart -ge $segment.end) { $next += $segment; continue }
            if ($windowStart -gt $segment.start) { $next += [ordered]@{ start = $segment.start; end = $windowStart } }
            if ($windowEnd -lt $segment.end) { $next += [ordered]@{ start = $windowEnd; end = $segment.end } }
        }
        $segments = $next
    }
    return @($segments)
}

function Get-ExpectedPlannedSeconds {
    param([Parameter(Mandatory)]$Collector, [Parameter(Mandatory)][datetime]$DayStartUTC, [Parameter(Mandatory)]$Excluded)
    $dayLocal = $DayStartUTC.AddHours(8)
    $dayName = $dayLocal.DayOfWeek.ToString().ToLowerInvariant()
    $seconds = [double]0
    foreach ($window in @($Collector.planned_schedule.windows | Where-Object { @($_.days) -contains $dayName })) {
        $date = $dayLocal.ToString('yyyy-MM-dd')
        $start = Convert-IsoLocalToUtc -LocalText "$date`T$($window.start_local)" -Offset '+08:00'
        $end = if ($window.end_local -eq '24:00') { $DayStartUTC.AddDays(1) } else { Convert-IsoLocalToUtc -LocalText "$date`T$($window.end_local)" -Offset '+08:00' }
        foreach ($segment in @(Get-CoverageSegments -Start $start -End $end -Excluded $Excluded)) { $seconds += ($segment.end - $segment.start).TotalSeconds }
    }
    return $seconds
}

function Measure-Coverage {
    param([Parameter(Mandatory)]$Coverage, [Parameter(Mandatory)]$Collectors, [Parameter(Mandatory)]$Excluded, [Parameter(Mandatory)][datetime]$DayStartUTC)
    $result = @()
    foreach ($collector in @($Collectors)) {
        $planned = [double]0
        $usable = [double]0
        $states = [ordered]@{ covered = 0.0; confirmed_idle = 0.0; pending = 0.0; delayed = 0.0; offline = 0.0; unknown = 0.0 }
        $unexpectedRun = [double]0
        $maxUnexpectedRun = [double]0
        $lastUnexpectedEnd = $null
        foreach ($interval in @($Coverage.intervals | Where-Object { $_.collector_id -eq $collector.id } | Sort-Object start_utc)) {
            $start = [DateTime]::Parse([string]$interval.start_utc).ToUniversalTime()
            $end = [DateTime]::Parse([string]$interval.end_utc).ToUniversalTime()
            $segments = Get-CoverageSegments -Start $start -End $end -Excluded $Excluded
            foreach ($segment in $segments) {
                $seconds = ($segment.end - $segment.start).TotalSeconds
                $planned += $seconds
                $states[[string]$interval.availability] += $seconds
                $flags = @($interval.quality_flags)
                $bad = $flags -contains 'corrupt' -or $flags -contains 'incomplete' -or ($collector.kind -eq 'media' -and $flags -contains 'obscured')
                $isUsable = $interval.availability -eq 'covered' -or ($collector.kind -eq 'activitywatch' -and $interval.availability -eq 'confirmed_idle')
                if ($isUsable -and -not $bad) { $usable += $seconds }
                if ($interval.availability -eq 'offline' -or $interval.availability -eq 'unknown') {
                    if ($null -ne $lastUnexpectedEnd -and $segment.start -gt $lastUnexpectedEnd) { $unexpectedRun = 0 }
                    $unexpectedRun += $seconds
                    $lastUnexpectedEnd = $segment.end
                    if ($unexpectedRun -gt $maxUnexpectedRun) { $maxUnexpectedRun = $unexpectedRun }
                }
                else { $unexpectedRun = 0; $lastUnexpectedEnd = $null }
            }
        }
        $projection = @($Coverage.projections | Where-Object { $_.collector_id -eq $collector.id } | Select-Object -First 1)
        $expectedPlanned = Get-ExpectedPlannedSeconds -Collector $collector -DayStartUTC $DayStartUTC -Excluded $Excluded
        $classificationPassed = [Math]::Abs($planned - $expectedPlanned) -lt 0.001
        $ratio = if ($planned -gt 0) { $usable / $planned } else { 0.0 }
        $limit = if ($collector.kind -eq 'activitywatch') { 300 } else { 900 }
        $result += [ordered]@{
            collector_id = $collector.id; kind = $collector.kind; planned_seconds = [int64][Math]::Round($planned)
            expected_planned_seconds = [int64][Math]::Round($expectedPlanned); classification_passed = $classificationPassed
            usable_seconds = [int64][Math]::Round($usable); usable_ratio = $ratio
            availability_seconds = $states; maximum_unexpected_offline_unknown_seconds = [int64][Math]::Round($maxUnexpectedRun)
            projection_status = if ($projection.Count -eq 1) { $projection[0].status } else { 'missing' }
            passed = ($planned -gt 0 -and $classificationPassed -and $ratio -ge 0.99 -and $maxUnexpectedRun -le $limit -and $projection.Count -eq 1 -and $projection[0].status -eq 'fresh')
        }
    }
    return $result
}

function Invoke-Initialize {
    foreach ($required in @($CertificationDirectory, $BinaryPath, $ConfigPath, $ReleaseManifestPath, $PreviousReleaseDirectory, $BackupDirectory, $ProfilePath)) {
        if ([string]::IsNullOrWhiteSpace($required)) { throw 'M6_INITIALIZE_ARGUMENT_MISSING' }
    }
    $certificationRoot = Assert-AbsolutePath -Path $CertificationDirectory -Code 'M6_CERTIFICATION_DIRECTORY_INVALID'
    if (Test-Path -LiteralPath $certificationRoot) { throw 'M6_CERTIFICATION_DIRECTORY_EXISTS' }
    $binary = Assert-RegularFile -Path $BinaryPath -Code 'M6_CANDIDATE_BINARY_INVALID'
    $config = Assert-RegularFile -Path $ConfigPath -Code 'M6_CONFIG_INVALID'
    $releaseManifestFile = Assert-RegularFile -Path $ReleaseManifestPath -Code 'M6_RELEASE_MANIFEST_INVALID'
    $previousRelease = Assert-RegularDirectory -Path $PreviousReleaseDirectory -Code 'M6_PREVIOUS_RELEASE_INVALID'
    $backupRoot = Assert-RegularDirectory -Path $BackupDirectory -Code 'M6_BACKUP_DIRECTORY_INVALID' -Create
    $profileFile = Assert-RegularFile -Path $ProfilePath -Code 'M6_PROFILE_INVALID'
    $profile = Read-JsonFile -Path $profileFile -Code 'M6_PROFILE_INVALID'
    if ($profile.schema_version -ne 1 -or $profile.utc_offset -ne '+08:00' -or $profile.sample_interval_minutes -lt 1 -or $profile.sample_interval_minutes -gt 15 -or $profile.media_sample_count -lt 1) { throw 'M6_PROFILE_INVALID' }
    foreach ($name in @('max_cpu_percent', 'max_working_set_bytes', 'max_private_bytes', 'max_handles', 'max_threads', 'max_goroutines', 'max_go_heap_bytes', 'max_go_runtime_system_bytes', 'max_wal_bytes', 'max_staging_bytes', 'max_staging_files', 'max_log_bytes', 'max_data_bytes', 'max_backup_bytes', 'max_media_ready_backlog', 'min_free_bytes')) {
        if ($null -eq $profile.limits.$name -or [double]$profile.limits.$name -le 0) { throw "M6_PROFILE_LIMIT_INVALID:$name" }
    }
    foreach ($name in @('core_process', 'system_reboot', 'activitywatch', 'media', 'backup_restore', 'rollback')) {
        if ($null -eq $profile.recovery_rto_seconds.$name -or [int]$profile.recovery_rto_seconds.$name -lt 1) { throw "M6_PROFILE_RTO_INVALID:$name" }
    }
    foreach ($kind in $requiredExerciseKinds) {
        if (@($profile.planned_exercises | Where-Object { $_.kind -eq $kind }).Count -ne 1) { throw "M6_PROFILE_EXERCISE_MISSING:$kind" }
    }
    foreach ($exercise in @($profile.planned_exercises)) {
        if ([string]::IsNullOrWhiteSpace([string]$exercise.rto_key) -or $null -eq $profile.recovery_rto_seconds.([string]$exercise.rto_key)) { throw "M6_PROFILE_EXERCISE_RTO_INVALID:$($exercise.kind)" }
    }
    $publisherProfile = $profile.media_publisher
    if ($null -eq $publisherProfile -or [string]::IsNullOrWhiteSpace([string]$publisherProfile.device_name) -or [string]$publisherProfile.device_name -match '["\r\n]' -or [string]::IsNullOrWhiteSpace([string]$publisherProfile.collector_id)) { throw 'M6_MEDIA_PUBLISHER_PROFILE_INVALID' }
    if ([int]$publisherProfile.segment_seconds -lt 1 -or [int]$publisherProfile.segment_seconds -gt 600 -or [int]$publisherProfile.segment_count -lt 1 -or [int]$publisherProfile.segment_count -gt 12 -or [int]$publisherProfile.clock_error_ms -lt 0 -or [int]$publisherProfile.acceptance_timeout_seconds -lt 5 -or [int]$publisherProfile.acceptance_timeout_seconds -gt 600) { throw 'M6_MEDIA_PUBLISHER_PROFILE_INVALID' }
    [void](Convert-LocalClockToMinutes -Value ([string]$publisherProfile.daily_start_local))

    $repositoryStatus = @(& git -C $sourceRepoRoot status --porcelain=v1 --untracked-files=all)
    if ($LASTEXITCODE -ne 0 -or $repositoryStatus.Count -ne 0) { throw 'M6_REPOSITORY_NOT_CLEAN' }
    $head = ((& git -C $sourceRepoRoot rev-parse HEAD) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'M6_REPOSITORY_INVALID' }
    $release = Read-JsonFile -Path $releaseManifestFile -Code 'M6_RELEASE_MANIFEST_INVALID'
    $version = Invoke-BinaryJson -Executable $binary -Arguments @('--version') -Code 'M6_CANDIDATE_VERSION_INVALID'
    if ($release.schema_version -ne 1 -or $release.commit -ne $head -or $release.commit -match '-dirty$' -or $version.commit -ne $head -or $version.version -ne $release.version -or (Get-SHA256 -Path $binary) -ne $release.binary.sha256) { throw 'M6_CANDIDATE_IDENTITY_MISMATCH' }
    $configCheck = Invoke-BinaryJson -Executable $binary -Arguments @("--config=$config", '--check-config') -Code 'M6_CONFIG_INVALID'
    $liveSchema = Invoke-BinaryJson -Executable $binary -Arguments @("--config=$config", '--schema-info') -Code 'M6_SCHEMA_INVALID'
    $configRaw = Read-JsonFile -Path $config -Code 'M6_CONFIG_INVALID'
    if ($configCheck.status -ne 'ok' -or $configRaw.runtime.mode -ne 'record-only' -or -not $configRaw.runtime.backup_interface_enabled -or $configRaw.retention.enabled -or -not $configRaw.media_ingest.enabled) { throw 'M6_RECORD_ONLY_CONFIG_INVALID' }
    $dataDirectory = [IO.Path]::GetFullPath([string]$configCheck.data_directory)
    if ((Test-PathContained -Root $sourceRepoRoot -Candidate $certificationRoot) -or (Test-PathContained -Root $sourceRepoRoot -Candidate $backupRoot)) { throw 'M6_RUNTIME_PATH_INSIDE_REPOSITORY' }
    if ((Test-PathContained -Root $dataDirectory -Candidate $certificationRoot) -or (Test-PathContained -Root $dataDirectory -Candidate $backupRoot) -or (Test-PathContained -Root $certificationRoot -Candidate $dataDirectory) -or (Test-PathContained -Root $backupRoot -Candidate $dataDirectory)) { throw 'M6_RUNTIME_PATH_OVERLAP' }
    if ((Test-PathContained -Root $certificationRoot -Candidate $backupRoot) -or (Test-PathContained -Root $backupRoot -Candidate $certificationRoot)) { throw 'M6_CERTIFICATION_BACKUP_OVERLAP' }
    $ffprobe = Assert-RegularFile -Path ([string]$configRaw.media_ingest.ffprobe_path) -Code 'M6_FFPROBE_INVALID'
    $ffprobeLine = ((& $ffprobe -version | Select-Object -First 1) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $ffprobeLine -notmatch "^ffprobe version $([regex]::Escape($requiredFFprobeVersion))\b") { throw 'M6_FFPROBE_VERSION_MISMATCH' }
    $enabledCollectors = @($configRaw.collectors | Where-Object { $_.enabled })
    $activityWatch = @($enabledCollectors | Where-Object { $_.kind -eq 'activitywatch' })
    $media = @($enabledCollectors | Where-Object { $_.kind -eq 'media' })
    if ($activityWatch.Count -lt 1 -or $media.Count -lt 1) { throw 'M6_CORE_COLLECTORS_MISSING' }
    foreach ($collector in @($activityWatch + $media)) {
        if ($collector.planned_schedule.timezone -ne 'Asia/Shanghai' -or @($collector.planned_schedule.windows).Count -eq 0) { throw "M6_COLLECTOR_SCHEDULE_INVALID:$($collector.id)" }
    }
    foreach ($collector in $activityWatch) {
        $base = [Uri]$collector.activitywatch.base_url
        if (-not $base.IsLoopback -or $base.Scheme -ne 'http') { throw "M6_ACTIVITYWATCH_ORIGIN_INVALID:$($collector.id)" }
        $bucket = [Uri]::EscapeDataString([string]$collector.activitywatch.bucket_id)
        $source = Invoke-LocalJson -Uri ($base.AbsoluteUri.TrimEnd('/') + "/api/0/buckets/$bucket") -Code "M6_ACTIVITYWATCH_UNAVAILABLE:$($collector.id)"
        if ([string]$source.id -ne [string]$collector.activitywatch.bucket_id) { throw "M6_ACTIVITYWATCH_BUCKET_MISMATCH:$($collector.id)" }
    }
    $publisherCollector = @($media | Where-Object { $_.id -eq [string]$publisherProfile.collector_id })
    if ($publisherCollector.Count -ne 1) { throw 'M6_MEDIA_PUBLISHER_COLLECTOR_INVALID' }
    $publisherStartMinutes = Convert-LocalClockToMinutes -Value ([string]$publisherProfile.daily_start_local)
    $publisherDurationSeconds = [int]$publisherProfile.segment_seconds * [int]$publisherProfile.segment_count
    $allDays = @('monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday')
    $publisherWindows = @($publisherCollector[0].planned_schedule.windows | Where-Object {
        $window = $_
        $windowStart = Convert-LocalClockToMinutes -Value ([string]$window.start_local)
        $windowEnd = Convert-LocalClockToMinutes -Value ([string]$window.end_local)
        @($allDays | Where-Object { @($window.days) -notcontains $_ }).Count -eq 0 -and $windowStart -eq $publisherStartMinutes -and (($windowEnd - $windowStart) * 60) -eq $publisherDurationSeconds
    })
    if ($publisherWindows.Count -ne 1) { throw 'M6_MEDIA_PUBLISHER_SCHEDULE_MISMATCH' }
    $ffmpeg = Assert-RegularFile -Path ([string]$publisherProfile.ffmpeg_path) -Code 'M6_FFMPEG_INVALID'
    $ffmpegLine = ((& $ffmpeg -version | Select-Object -First 1) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $ffmpegLine -notmatch "^ffmpeg version $([regex]::Escape($requiredFFmpegVersion))\b") { throw 'M6_FFMPEG_VERSION_MISMATCH' }
    $publisherSource = Assert-RegularFile -Path (Join-Path $sourceRepoRoot 'scripts\m6-desk-media.ps1') -Code 'M6_MEDIA_PUBLISHER_TOOL_MISSING'

    $releaseDirectory = Split-Path -Parent $binary
    $appRoot = Split-Path -Parent (Split-Path -Parent $releaseDirectory)
    $pointer = Read-JsonFile -Path (Join-Path $appRoot 'current.json') -Code 'M6_CURRENT_POINTER_INVALID'
    if ([IO.Path]::GetFullPath([string]$pointer.release_directory) -ne [IO.Path]::GetFullPath($releaseDirectory)) { throw 'M6_CANDIDATE_NOT_INSTALLED' }
    $previousManifest = Assert-RegularFile -Path (Join-Path $previousRelease 'release-manifest.json') -Code 'M6_PREVIOUS_RELEASE_INVALID'
    $previousBinary = Assert-RegularFile -Path (Join-Path $previousRelease 'exam-monitor.exe') -Code 'M6_PREVIOUS_RELEASE_INVALID'
    $previous = Read-JsonFile -Path $previousManifest -Code 'M6_PREVIOUS_RELEASE_INVALID'
    if ((Get-SHA256 -Path $previousBinary) -ne $previous.binary.sha256 -or $previous.version -eq $release.version) { throw 'M6_PREVIOUS_RELEASE_INVALID' }
    foreach ($ledger in @('core', 'media', 'm3', 'm4')) {
        $versionValue = [int]$liveSchema.$ledger
        if ($versionValue -lt [int]$release.database_schema.$ledger.minimum -or $versionValue -gt [int]$release.database_schema.$ledger.maximum -or $versionValue -lt [int]$previous.database_schema.$ledger.minimum -or $versionValue -gt [int]$previous.database_schema.$ledger.maximum) { throw "M6_SCHEMA_COMPATIBILITY_INVALID:$ledger" }
    }
    $baseURL = 'http://' + [string]$configCheck.listen_address
    $live = Invoke-LocalJson -Uri "$baseURL/health/live" -Code 'M6_CORE_UNAVAILABLE'
    $ready = Invoke-LocalJson -Uri "$baseURL/health/ready" -Code 'M6_CORE_UNAVAILABLE'
    if ($live.version -ne $release.version -or $live.mode -ne 'record-only' -or $ready.status -ne 'writable') { throw 'M6_CORE_NOT_READY' }
    $collectorStatus = Invoke-LocalJson -Uri "$baseURL/api/v1/collectors/status" -Code 'M6_CORE_COLLECTORS_UNAVAILABLE'
    foreach ($collector in $activityWatch) {
        $status = @($collectorStatus.collectors | Where-Object { $_.collector_id -eq $collector.id })
        if ($status.Count -ne 1 -or $status[0].status -ne 'healthy' -or [string]::IsNullOrWhiteSpace([string]$status[0].last_success_utc)) { throw "M6_ACTIVITYWATCH_NOT_PRODUCING:$($collector.id)" }
    }
    $mediaStatus = Invoke-LocalJson -Uri "$baseURL/api/v1/media/ingest/status" -Code 'M6_MEDIA_UNAVAILABLE'
    if ($mediaStatus.status -ne 'healthy' -or [int64]$mediaStatus.ingest.total_segments -lt 1 -or [int64]$mediaStatus.filesystem_ready_backlog -ne 0) { throw 'M6_MEDIA_NOT_PRODUCING' }

    $testOutput = @(& (Join-Path $sourceRepoRoot 'scripts\test.ps1') 2>&1)
    if ($LASTEXITCODE -ne 0) { throw 'M6_PREFLIGHT_TEST_FAILED' }
    $faultOutput = @(& (Join-Path $sourceRepoRoot 'scripts\fault-injection.ps1') -BinaryPath $binary 2>&1)
    if ($LASTEXITCODE -ne 0) { throw 'M6_PREFLIGHT_FAULT_INJECTION_FAILED' }

    [void](New-Item -ItemType Directory -Path $certificationRoot)
    foreach ($name in @('samples', 'daily', 'backups', 'records', 'preflight', 'tool', 'state', 'media-publisher')) { [void](New-Item -ItemType Directory -Path (Join-Path $certificationRoot $name)) }
    $frozenTool = Join-Path $certificationRoot 'tool\m6-certification.ps1'
    $frozenPublisher = Join-Path $certificationRoot 'tool\m6-desk-media.ps1'
    Copy-Item -LiteralPath $PSCommandPath -Destination $frozenTool
    Copy-Item -LiteralPath $publisherSource -Destination $frozenPublisher
    $publisherPlanOutput = @(& $publisherSource -InboxDirectory $configCheck.media_inbox_directory -FFmpegPath $ffmpeg -ExpectedFFmpegSHA256 (Get-SHA256 -Path $ffmpeg) -DeviceName $publisherProfile.device_name -StateDirectory (Join-Path $certificationRoot 'media-publisher') -CollectorID $publisherProfile.collector_id -SegmentSeconds $publisherProfile.segment_seconds -SegmentCount $publisherProfile.segment_count -ClockErrorMS $publisherProfile.clock_error_ms -AcceptanceTimeoutSeconds $publisherProfile.acceptance_timeout_seconds -PlanOnly)
    $publisherPlan = (($publisherPlanOutput | Out-String).Trim() | ConvertFrom-Json)
    Write-JsonAtomic -Path (Join-Path $certificationRoot 'preflight\media-publisher-plan.json') -Value $publisherPlan
    $startLocalDate = [DateTime]::UtcNow.AddHours(8).Date.AddDays(1)
    $startUTC = Convert-IsoLocalToUtc -LocalText $startLocalDate.ToString('yyyy-MM-ddT00:00') -Offset $profile.utc_offset
    $endUTC = $startUTC.AddDays(14)
    $planned = @()
    foreach ($exercise in @($profile.planned_exercises)) {
        if ($exercise.relative_day -lt 1 -or $exercise.relative_day -gt 14 -or $exercise.duration_minutes -lt 1 -or $exercise.duration_minutes -gt 60) { throw 'M6_PROFILE_EXERCISE_INVALID' }
        $localDate = $startLocalDate.AddDays([int]$exercise.relative_day - 1).ToString('yyyy-MM-dd')
        $windowStart = Convert-IsoLocalToUtc -LocalText "$localDate`T$($exercise.start_local)" -Offset $profile.utc_offset
        $planned += [ordered]@{
            id = $exercise.id; kind = $exercise.kind; relative_day = $exercise.relative_day; start_utc = $windowStart.ToString('o')
            end_utc = $windowStart.AddMinutes([int]$exercise.duration_minutes).ToString('o'); rto_key = $exercise.rto_key
            rto_seconds = [int]$profile.recovery_rto_seconds.([string]$exercise.rto_key)
        }
    }
    $collectors = @()
    foreach ($collector in $enabledCollectors) {
        $collectors += [ordered]@{
            id = $collector.id; kind = $collector.kind; heartbeat_period = $collector.heartbeat_period
            allowed_lateness = $collector.allowed_lateness; offline_after = $collector.offline_after
            planned_schedule = $collector.planned_schedule
        }
    }
    $dependencyFiles = @('go.mod', 'go.sum', 'vendor\modules.txt', 'web\package.json')
    $dependencies = @()
    foreach ($relative in $dependencyFiles) {
        $path = Assert-RegularFile -Path (Join-Path $sourceRepoRoot $relative) -Code 'M6_DEPENDENCY_LOCK_MISSING'
        $dependencies += [ordered]@{ path = $relative.Replace('\', '/'); sha256 = Get-SHA256 -Path $path }
    }
    $manifest = [ordered]@{
        schema_version = 1; certification_id = [guid]::NewGuid().ToString('N'); created_at_utc = [DateTime]::UtcNow.ToString('o')
        starts_at_utc = $startUTC.ToString('o'); ends_at_utc = $endUTC.ToString('o'); required_consecutive_days = 14
        candidate = [ordered]@{
            version = $release.version; commit = $release.commit; build_time_utc = $release.build_time_utc
            binary_path = $binary; binary_sha256 = Get-SHA256 -Path $binary
            release_directory = $releaseDirectory; release_manifest_path = $releaseManifestFile; release_manifest_sha256 = Get-SHA256 -Path $releaseManifestFile
            database_schema = $liveSchema; supported_database_schema = $release.database_schema
        }
        previous_stable = [ordered]@{ version = $previous.version; release_directory = $previousRelease; binary_sha256 = $previous.binary.sha256; release_manifest_sha256 = Get-SHA256 -Path $previousManifest; supported_database_schema = $previous.database_schema }
        configuration = [ordered]@{
            path = $config; sha256 = Get-SHA256 -Path $config; mode = 'record-only'; retention_enabled = $false
            listen_address = $configCheck.listen_address; data_directory = $configCheck.data_directory; database_path = $configCheck.database_path
            media_inbox_directory = $configCheck.media_inbox_directory; ffprobe_path = $ffprobe; ffprobe_version = $requiredFFprobeVersion
            storage = $configRaw.storage; operations = $configRaw.operations; retention = $configRaw.retention
        }
        collectors = $collectors; dependencies = $dependencies; vendor_tree_sha256 = Get-DependencyTreeHash -Root (Join-Path $sourceRepoRoot 'vendor')
        media_publisher = [ordered]@{
            tool_path = $frozenPublisher; tool_sha256 = Get-SHA256 -Path $frozenPublisher; ffmpeg_path = $ffmpeg; ffmpeg_sha256 = Get-SHA256 -Path $ffmpeg; ffmpeg_version = $requiredFFmpegVersion
            device_name = $publisherProfile.device_name; collector_id = $publisherProfile.collector_id; daily_start_local = $publisherProfile.daily_start_local
            segment_seconds = [int]$publisherProfile.segment_seconds; segment_count = [int]$publisherProfile.segment_count
            clock_error_ms = [int64]$publisherProfile.clock_error_ms; acceptance_timeout_seconds = [int]$publisherProfile.acceptance_timeout_seconds
        }
        limits = $profile.limits; backup = [ordered]@{ directory = $backupRoot; rpo_hours = $profile.backup_rpo_hours; media_sample_count = $profile.media_sample_count }
        recovery_rto_seconds = $profile.recovery_rto_seconds; planned_exercises = $planned
        automation = [ordered]@{ sample_interval_minutes = $profile.sample_interval_minutes; backup_local_time = '00:10'; daily_local_time = '01:00'; backup_schedule_tolerance_seconds = 300 }
        paths = [ordered]@{ repository_root = $sourceRepoRoot; app_root = $appRoot; certification_directory = $certificationRoot; backup_directory = $backupRoot }
        tasks = [ordered]@{ sample = "ExamMonitor M6 Sample $($release.version)"; media = "ExamMonitor M6 Desk Media $($release.version)"; backup = "ExamMonitor M6 Backup $($release.version)"; daily = "ExamMonitor M6 Daily $($release.version)" }
        environment = [ordered]@{ machine = $env:COMPUTERNAME; user = [Security.Principal.WindowsIdentity]::GetCurrent().Name; os = [Environment]::OSVersion.VersionString; powershell = $PSVersionTable.PSVersion.ToString() }
        tool = [ordered]@{ path = $frozenTool; sha256 = Get-SHA256 -Path $frozenTool }
    }
    $manifestPath = Join-Path $certificationRoot 'freeze-manifest.json'
    Write-JsonAtomic -Path $manifestPath -Value $manifest
    [IO.File]::WriteAllText((Join-Path $certificationRoot 'freeze-manifest.sha256'), ((Get-SHA256 -Path $manifestPath) + [Environment]::NewLine), $utf8NoBom)
    Write-JsonAtomic -Path (Join-Path $certificationRoot 'preflight\source-status.json') -Value ([ordered]@{ checked_at_utc = [DateTime]::UtcNow.ToString('o'); live = $live; ready = $ready; collectors = $collectorStatus; media = $mediaStatus; activitywatch_collectors = $activityWatch.Count; media_collectors = $media.Count })
    [IO.File]::WriteAllLines((Join-Path $certificationRoot 'preflight\test.txt'), @($testOutput | ForEach-Object { [string]$_ }), $utf8NoBom)
    [IO.File]::WriteAllLines((Join-Path $certificationRoot 'preflight\fault-injection.txt'), @($faultOutput | ForEach-Object { [string]$_ }), $utf8NoBom)
    $baselineBackupOutput = @(& (Join-Path $sourceRepoRoot 'scripts\backup.ps1') -BinaryPath $binary -ConfigPath $config -DestinationDirectory $backupRoot -Type full)
    if ($LASTEXITCODE -ne 0) { throw 'M6_PREFLIGHT_BACKUP_FAILED' }
    $baselineBackup = (($baselineBackupOutput | Out-String).Trim() | ConvertFrom-Json)
    Write-JsonAtomic -Path (Join-Path $certificationRoot 'preflight\baseline-backup.json') -Value $baselineBackup
    $baselineDatabasePath = Join-Path $baselineBackup.backup_directory 'database\exam-monitor.db'
    $baselineDatabase = Invoke-BinaryJson -Executable $binary -Arguments @("--certification-snapshot-database=$baselineDatabasePath") -Code 'M6_PREFLIGHT_DATABASE_FAILED'
    Write-JsonAtomic -Path (Join-Path $certificationRoot 'preflight\baseline-database.json') -Value $baselineDatabase
    $restoreTarget = Join-Path $certificationRoot 'preflight\restored'
    $restoreOutput = @(& (Join-Path $sourceRepoRoot 'scripts\restore.ps1') -BackupDirectory $baselineBackup.backup_directory -VerifierBinaryPath $binary -TargetDirectory $restoreTarget)
    if ($LASTEXITCODE -ne 0) { throw 'M6_PREFLIGHT_RESTORE_FAILED' }
    $restoreResult = (($restoreOutput | Out-String).Trim() | ConvertFrom-Json)
    Write-JsonAtomic -Path (Join-Path $certificationRoot 'preflight\baseline-restore.json') -Value $restoreResult
    return $manifest
}

function Invoke-InstallTasks {
    param([Parameter(Mandatory)]$Manifest)
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().Name
    $command = [Security.SecurityElement]::Escape($PSHOME + '\powershell.exe')
    $tool = [Security.SecurityElement]::Escape([string]$Manifest.tool.path)
    $root = [Security.SecurityElement]::Escape([string]$Manifest.paths.certification_directory)
    $publisherTool = [Security.SecurityElement]::Escape([string]$Manifest.media_publisher.tool_path)
    $publisherInbox = [Security.SecurityElement]::Escape([string]$Manifest.configuration.media_inbox_directory)
    $publisherFFmpeg = [Security.SecurityElement]::Escape([string]$Manifest.media_publisher.ffmpeg_path)
    $publisherDevice = [Security.SecurityElement]::Escape([string]$Manifest.media_publisher.device_name)
    $publisherState = [Security.SecurityElement]::Escape((Join-Path $Manifest.paths.certification_directory 'media-publisher'))
    $publisherCollector = [Security.SecurityElement]::Escape([string]$Manifest.media_publisher.collector_id)
    $start = [DateTime]::Now.AddMinutes(1).ToString('s')
    $certificationStartLocal = [DateTime]::Parse([string]$Manifest.starts_at_utc).ToUniversalTime().AddHours(8)
    $backupStart = $certificationStartLocal.Date.AddMinutes(10).ToString('s')
    $mediaStart = $certificationStartLocal.Date.AddMinutes((Convert-LocalClockToMinutes -Value ([string]$Manifest.media_publisher.daily_start_local))).ToString('s')
    $dailyStart = $certificationStartLocal.Date.AddDays(1).AddHours(1).ToString('s')
    $sampleMinutes = [int]$Manifest.automation.sample_interval_minutes
    $tasks = @(
        [ordered]@{ name = $Manifest.tasks.sample; limit = 'PT5M'; start_when_available = 'true'; trigger = "<TimeTrigger><StartBoundary>$start</StartBoundary><Repetition><Interval>PT${sampleMinutes}M</Interval></Repetition></TimeTrigger>"; action = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File &quot;$tool&quot; -Action Sample -CertificationDirectory &quot;$root&quot;" },
        [ordered]@{ name = $Manifest.tasks.media; limit = 'PT45M'; start_when_available = 'false'; trigger = "<CalendarTrigger><StartBoundary>$mediaStart</StartBoundary><Enabled>true</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger>"; action = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File &quot;$publisherTool&quot; -InboxDirectory &quot;$publisherInbox&quot; -FFmpegPath &quot;$publisherFFmpeg&quot; -ExpectedFFmpegSHA256 $($Manifest.media_publisher.ffmpeg_sha256) -DeviceName &quot;$publisherDevice&quot; -StateDirectory &quot;$publisherState&quot; -CollectorID &quot;$publisherCollector&quot; -SegmentSeconds $([int]$Manifest.media_publisher.segment_seconds) -SegmentCount $([int]$Manifest.media_publisher.segment_count) -ClockErrorMS $([int64]$Manifest.media_publisher.clock_error_ms) -AcceptanceTimeoutSeconds $([int]$Manifest.media_publisher.acceptance_timeout_seconds)" },
        [ordered]@{ name = $Manifest.tasks.backup; limit = 'PT45M'; start_when_available = 'true'; trigger = "<CalendarTrigger><StartBoundary>$backupStart</StartBoundary><Enabled>true</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger>"; action = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File &quot;$tool&quot; -Action Backup -CertificationDirectory &quot;$root&quot;" },
        [ordered]@{ name = $Manifest.tasks.daily; limit = 'PT45M'; start_when_available = 'true'; trigger = "<CalendarTrigger><StartBoundary>$dailyStart</StartBoundary><Enabled>true</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger>"; action = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File &quot;$tool&quot; -Action Daily -CertificationDirectory &quot;$root&quot;" }
    )
    foreach ($task in $tasks) {
        $xml = @"
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Exam Monitor M6 frozen certification evidence task</Description></RegistrationInfo>
  <Triggers>$($task.trigger)</Triggers>
  <Principals><Principal id="Author"><UserId>$([Security.SecurityElement]::Escape($identity))</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>$($task.start_when_available)</StartWhenAvailable><ExecutionTimeLimit>$($task.limit)</ExecutionTimeLimit><Enabled>true</Enabled></Settings>
  <Actions Context="Author"><Exec><Command>$command</Command><Arguments>$($task.action)</Arguments></Exec></Actions>
</Task>
"@
        $temporary = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor-m6-task-" + [guid]::NewGuid().ToString('N') + '.xml')
        try {
            [IO.File]::WriteAllText($temporary, $xml, [Text.Encoding]::Unicode)
            $savedPreference = $ErrorActionPreference
            $ErrorActionPreference = 'Continue'
            try { $taskOutput = @(& "$env:SystemRoot\System32\schtasks.exe" /Create /TN $task.name /XML $temporary /F 2>&1); $taskExit = $LASTEXITCODE }
            finally { $ErrorActionPreference = $savedPreference }
            if ($taskExit -ne 0) { throw "M6_TASK_INSTALL_FAILED:$($task.name):$taskExit`:$((($taskOutput | Out-String).Trim()))" }
        }
        finally { Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue }
    }
}

function Invoke-RemoveTasks {
    param([Parameter(Mandatory)]$Manifest)
    foreach ($name in @($Manifest.tasks.sample, $Manifest.tasks.media, $Manifest.tasks.backup, $Manifest.tasks.daily)) {
        $savedPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try { & "$env:SystemRoot\System32\schtasks.exe" /Delete /TN $name /F 2>&1 | Out-Null; $taskExit = $LASTEXITCODE }
        finally { $ErrorActionPreference = $savedPreference }
        if ($taskExit -notin @(0, 1)) { throw "M6_TASK_REMOVE_FAILED:$name`:$taskExit" }
    }
}

function Invoke-CertificationBackup {
    param([Parameter(Mandatory)]$Manifest)
    Assert-FrozenInputs -Manifest $Manifest
    $repositoryRoot = [string]$Manifest.paths.repository_root
    $output = @(& (Join-Path $repositoryRoot 'scripts\backup.ps1') -BinaryPath $Manifest.candidate.binary_path -ConfigPath $Manifest.configuration.path -DestinationDirectory $Manifest.backup.directory -Type full)
    if ($LASTEXITCODE -ne 0) { throw 'M6_BACKUP_FAILED' }
    $result = (($output | Out-String).Trim() | ConvertFrom-Json)
    $backupManifest = Read-JsonFile -Path $result.manifest_path -Code 'M6_BACKUP_INVALID'
    $record = [ordered]@{
        schema_version = 1; created_at_utc = $backupManifest.created_at_utc; completed_at_utc = [DateTime]::UtcNow.ToString('o')
        status = $result.status; type = $result.type; backup_directory = $result.backup_directory; manifest_path = $result.manifest_path
    }
    $name = [DateTime]::Parse([string]$record.created_at_utc).ToUniversalTime().ToString('yyyyMMddTHHmmssfffffffZ') + '.json'
    Write-JsonAtomic -Path (Join-Path $Manifest.paths.certification_directory "backups\$name") -Value $record
    return $record
}

function Read-CertificationBackupRecords {
    param([Parameter(Mandatory)]$Manifest)
    $directory = Join-Path $Manifest.paths.certification_directory 'backups'
    $records = @()
    foreach ($file in @(Get-ChildItem -LiteralPath $directory -File -Filter '*.json' -ErrorAction SilentlyContinue)) {
        $records += Read-JsonFile -Path $file.FullName -Code 'M6_BACKUP_RECORD_INVALID'
    }
    return @($records | Sort-Object { [DateTime]::Parse([string]$_.created_at_utc).ToUniversalTime() })
}

function Invoke-Sample {
    param([Parameter(Mandatory)]$Manifest)
    Assert-FrozenInputs -Manifest $Manifest
    $root = [string]$Manifest.paths.certification_directory
    $baseURL = 'http://' + [string]$Manifest.configuration.listen_address
    $live = Invoke-LocalJsonOrUnavailable -Uri "$baseURL/health/live"
    $ready = Invoke-LocalJsonOrUnavailable -Uri "$baseURL/health/ready"
    $operations = Invoke-LocalJsonOrUnavailable -Uri "$baseURL/api/v1/operations/status"
    $media = Invoke-LocalJsonOrUnavailable -Uri "$baseURL/api/v1/media/ingest/status"
    $collectors = Invoke-LocalJsonOrUnavailable -Uri "$baseURL/api/v1/collectors/status"
    $supervisorPath = Join-Path $Manifest.paths.app_root 'state\supervisor-state.json'
    $supervisor = if (Test-Path -LiteralPath $supervisorPath -PathType Leaf) { Read-JsonFile -Path $supervisorPath -Code 'M6_SUPERVISOR_STATE_INVALID' } else { [ordered]@{ status = 'missing'; crash_times_utc = @() } }
    $process = $null
    if ($null -ne $supervisor.child_pid) { $process = Get-Process -Id ([int]$supervisor.child_pid) -ErrorAction SilentlyContinue }
    $processMetrics = [ordered]@{ pid = $null; cpu_total_seconds = $null; cpu_percent = $null; working_set_bytes = $null; private_bytes = $null; handles = $null; threads = $null }
    $statePath = Join-Path $root 'state\sample-state.json'
    $previous = if (Test-Path -LiteralPath $statePath -PathType Leaf) { Read-JsonFile -Path $statePath -Code 'M6_SAMPLE_STATE_INVALID' } else { $null }
    $now = [DateTime]::UtcNow
    if ($null -ne $process) {
        $processMetrics.pid = $process.Id; $processMetrics.cpu_total_seconds = $process.TotalProcessorTime.TotalSeconds
        $processMetrics.working_set_bytes = $process.WorkingSet64; $processMetrics.private_bytes = $process.PrivateMemorySize64
        $processMetrics.handles = $process.HandleCount; $processMetrics.threads = $process.Threads.Count
        if ($null -ne $previous -and $previous.pid -eq $process.Id) {
            $elapsed = ($now - [DateTime]::Parse([string]$previous.at_utc).ToUniversalTime()).TotalSeconds
            if ($elapsed -gt 0) { $processMetrics.cpu_percent = [Math]::Max(0, 100 * ($processMetrics.cpu_total_seconds - [double]$previous.cpu_total_seconds) / ($elapsed * [Environment]::ProcessorCount)) }
        }
        Write-JsonAtomic -Path $statePath -Value ([ordered]@{ at_utc = $now.ToString('o'); pid = $process.Id; cpu_total_seconds = $processMetrics.cpu_total_seconds })
    }
    $database = [string]$Manifest.configuration.database_path
    $walPath = "$database-wal"
    $logMetrics = Get-TreeMeasurement -Path (Join-Path $Manifest.configuration.data_directory 'logs')
    $stagingMetrics = Get-TreeMeasurement -Path (Join-Path $Manifest.configuration.data_directory 'media\staging')
    $dataMetrics = Get-TreeMeasurement -Path $Manifest.configuration.data_directory
    $sample = [ordered]@{
        schema_version = 1; sampled_at_utc = $now.ToString('o'); system_boot_utc = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')
        live = $live; ready = $ready; operations = $operations; media = $media; collectors = if ($null -ne $collectors.PSObject.Properties['collectors']) { @($collectors.collectors) } else { @() }
        supervisor = $supervisor; process = $processMetrics
        files = [ordered]@{
            database_bytes = if (Test-Path -LiteralPath $database) { (Get-Item -LiteralPath $database).Length } else { 0 }
            wal_bytes = if (Test-Path -LiteralPath $walPath) { (Get-Item -LiteralPath $walPath).Length } else { 0 }
            logs = $logMetrics; staging = $stagingMetrics; data = $dataMetrics
        }
    }
    $day = $now.AddHours(8).ToString('yyyy-MM-dd')
    Append-JsonLine -Path (Join-Path $root "samples\$day.jsonl") -Value $sample
    return $sample
}

function Invoke-Daily {
    param([Parameter(Mandatory)]$Manifest, [string]$RequestedDate)
    Assert-FrozenInputs -Manifest $Manifest
    Assert-FrozenRepository -Manifest $Manifest
    $root = [string]$Manifest.paths.certification_directory
    $targetDate = if ($RequestedDate) { [DateTime]::ParseExact($RequestedDate, 'yyyy-MM-dd', [Globalization.CultureInfo]::InvariantCulture) } else { [DateTime]::UtcNow.AddHours(8).Date.AddDays(-1) }
    $dateText = $targetDate.ToString('yyyy-MM-dd')
    $dayStartUTC = Convert-IsoLocalToUtc -LocalText "$dateText`T00:00" -Offset '+08:00'
    $dayEndUTC = $dayStartUTC.AddDays(1)
    if ($dayStartUTC -lt [DateTime]::Parse([string]$Manifest.starts_at_utc).ToUniversalTime() -or $dayEndUTC -gt [DateTime]::Parse([string]$Manifest.ends_at_utc).ToUniversalTime()) { throw 'M6_DAILY_DATE_OUTSIDE_WINDOW' }
    $dailyPath = Join-Path $root "daily\$dateText.json"
    if (Test-Path -LiteralPath $dailyPath) { throw 'M6_DAILY_REPORT_EXISTS' }
    $backupRecords = @(Read-CertificationBackupRecords -Manifest $Manifest)
    $backup = @($backupRecords | Where-Object { [DateTime]::Parse([string]$_.created_at_utc).ToUniversalTime() -ge $dayEndUTC } | Select-Object -First 1)
    if ($backup.Count -eq 0) { $backup = @(Invoke-CertificationBackup -Manifest $Manifest); $backupRecords = @(Read-CertificationBackupRecords -Manifest $Manifest) }
    $backup = $backup[0]
    $backupCreatedUTC = [DateTime]::Parse([string]$backup.created_at_utc).ToUniversalTime()
    $previousBackup = @($backupRecords | Where-Object { [DateTime]::Parse([string]$_.created_at_utc).ToUniversalTime() -lt $backupCreatedUTC } | Select-Object -Last 1)
    $backupGapSeconds = $null
    $backupRPOPassed = $false
    if ($previousBackup.Count -eq 1) {
        $backupGapSeconds = ($backupCreatedUTC - [DateTime]::Parse([string]$previousBackup[0].created_at_utc).ToUniversalTime()).TotalSeconds
        $backupRPOPassed = $backupCreatedUTC -le $dayEndUTC.AddHours(1) -and $backupGapSeconds -le ([double]$Manifest.backup.rpo_hours * 3600 + [double]$Manifest.automation.backup_schedule_tolerance_seconds)
    }
    $backupManifest = Read-JsonFile -Path $backup.manifest_path -Code 'M6_DAILY_BACKUP_INVALID'
    $databasePath = Join-Path $backup.backup_directory 'database\exam-monitor.db'
    $database = Invoke-BinaryJson -Executable $Manifest.candidate.binary_path -Arguments @("--certification-snapshot-database=$databasePath") -Code 'M6_DAILY_DATABASE_FAILED'
    $baseURL = 'http://' + [string]$Manifest.configuration.listen_address
    $startText = [Uri]::EscapeDataString($dayStartUTC.ToString('o'))
    $endText = [Uri]::EscapeDataString($dayEndUTC.ToString('o'))
    $coverage = Invoke-LocalJson -Uri "$baseURL/api/v1/coverage?start=$startText&end=$endText" -Code 'M6_DAILY_COVERAGE_FAILED'
    $excluded = @($Manifest.planned_exercises | Where-Object { [DateTime]::Parse([string]$_.start_utc).ToUniversalTime() -lt $dayEndUTC -and [DateTime]::Parse([string]$_.end_utc).ToUniversalTime() -gt $dayStartUTC })
    $coverageSummary = Measure-Coverage -Coverage $coverage -Collectors $Manifest.collectors -Excluded $excluded -DayStartUTC $dayStartUTC
    $samplePath = Join-Path $root "samples\$dateText.jsonl"
    $samples = @()
    if (Test-Path -LiteralPath $samplePath -PathType Leaf) {
        foreach ($line in Get-Content -Encoding UTF8 -LiteralPath $samplePath) { if ($line.Trim()) { $samples += ($line | ConvertFrom-Json) } }
    }
    $peak = [ordered]@{}
    $selectors = [ordered]@{
        cpu_percent = { param($x) $x.process.cpu_percent }; working_set_bytes = { param($x) $x.process.working_set_bytes }
        private_bytes = { param($x) $x.process.private_bytes }; handles = { param($x) $x.process.handles }; threads = { param($x) $x.process.threads }
        goroutines = { param($x) if ($null -ne $x.operations.PSObject.Properties['runtime']) { $x.operations.runtime.goroutines } else { $null } }
        go_heap_bytes = { param($x) if ($null -ne $x.operations.PSObject.Properties['runtime']) { $x.operations.runtime.heap_alloc_bytes } else { $null } }
        go_runtime_system_bytes = { param($x) if ($null -ne $x.operations.PSObject.Properties['runtime']) { $x.operations.runtime.runtime_system_bytes } else { $null } }
        wal_bytes = { param($x) $x.files.wal_bytes }
        staging_bytes = { param($x) $x.files.staging.bytes }; staging_files = { param($x) $x.files.staging.files }; log_bytes = { param($x) $x.files.logs.bytes }
        data_bytes = { param($x) $x.files.data.bytes }; media_ready_backlog = { param($x) $x.media.filesystem_ready_backlog }
    }
    foreach ($name in $selectors.Keys) {
        $values = @($samples | ForEach-Object { & $selectors[$name] $_ } | Where-Object { $null -ne $_ })
        $peak[$name] = if ($values.Count -gt 0) { ($values | Measure-Object -Maximum).Maximum } else { $null }
    }
    $sampleTimes = @($samples | ForEach-Object { [DateTime]::Parse([string]$_.sampled_at_utc).ToUniversalTime() } | Sort-Object)
    $gapBoundaries = @($dayStartUTC) + $sampleTimes + @($dayEndUTC)
    $maximumSampleGapSeconds = [double]0
    for ($index = 1; $index -lt $gapBoundaries.Count; $index++) {
        foreach ($segment in @(Get-CoverageSegments -Start $gapBoundaries[$index - 1] -End $gapBoundaries[$index] -Excluded $excluded)) {
            $gap = ($segment.end - $segment.start).TotalSeconds
            if ($gap -gt $maximumSampleGapSeconds) { $maximumSampleGapSeconds = $gap }
        }
    }
    $mediaSamples = @()
    $mediaEntries = @($backupManifest.files | Where-Object { $_.kind -eq 'media' -and $_.included } | Sort-Object sha256 | Select-Object -First ([int]$Manifest.backup.media_sample_count))
    foreach ($entry in $mediaEntries) {
        $path = Join-Path $backup.backup_directory ([string]$entry.backup_path).Replace('/', '\')
        $mediaSamples += [ordered]@{ backup_path = $entry.backup_path; size_bytes = (Get-Item -LiteralPath $path).Length; sha256 = Get-SHA256 -Path $path; passed = ((Get-SHA256 -Path $path) -eq $entry.sha256) }
    }
    $limits = $Manifest.limits
    $backupMeasurement = Get-TreeMeasurement -Path $Manifest.backup.directory
    $minimumFreeBytesValues = @($samples | ForEach-Object { $_.operations.free_bytes } | Where-Object { $null -ne $_ })
    $minimumFreeBytes = if ($minimumFreeBytesValues.Count -gt 0) { ($minimumFreeBytesValues | Measure-Object -Minimum).Minimum } else { $null }
    $resourcesPassed = $true
    foreach ($check in @(
        @('cpu_percent', 'max_cpu_percent'), @('working_set_bytes', 'max_working_set_bytes'), @('private_bytes', 'max_private_bytes'),
        @('handles', 'max_handles'), @('threads', 'max_threads'), @('goroutines', 'max_goroutines'), @('go_heap_bytes', 'max_go_heap_bytes'),
        @('go_runtime_system_bytes', 'max_go_runtime_system_bytes'), @('wal_bytes', 'max_wal_bytes'), @('staging_bytes', 'max_staging_bytes'),
        @('staging_files', 'max_staging_files'), @('log_bytes', 'max_log_bytes'), @('data_bytes', 'max_data_bytes'),
        @('media_ready_backlog', 'max_media_ready_backlog')
    )) {
        if ($null -eq $peak[$check[0]] -or [double]$peak[$check[0]] -gt [double]$limits.($check[1])) { $resourcesPassed = $false }
    }
    if ($backupMeasurement.bytes -gt [int64]$limits.max_backup_bytes -or $null -eq $minimumFreeBytes -or [int64]$minimumFreeBytes -lt [int64]$limits.min_free_bytes) { $resourcesPassed = $false }
    $unexpectedUnhealthySamples = @($samples | Where-Object {
        $sampleTime = [DateTime]::Parse([string]$_.sampled_at_utc).ToUniversalTime()
        $insideExercise = @($excluded | Where-Object { $sampleTime -ge [DateTime]::Parse([string]$_.start_utc).ToUniversalTime() -and $sampleTime -lt [DateTime]::Parse([string]$_.end_utc).ToUniversalTime() }).Count -gt 0
        -not $insideExercise -and ($_.live.status -ne 'ok' -or $_.live.version -ne $Manifest.candidate.version -or $_.live.mode -ne 'record-only' -or $_.ready.status -ne 'writable')
    })
    $maximumAllowedSampleGapSeconds = [int]$Manifest.automation.sample_interval_minutes * 180
    $sampleHealth = $samples.Count -gt 0 -and $unexpectedUnhealthySamples.Count -eq 0 -and $maximumSampleGapSeconds -le $maximumAllowedSampleGapSeconds
    $duplicatesPassed = $database.potential_duplicates.raw_event_identity_groups -eq 0 -and $database.potential_duplicates.heartbeat_identity_groups -eq 0 -and $database.potential_duplicates.media_sha256_groups -eq 0
    $coveragePassed = $coverageSummary.Count -eq @($Manifest.collectors).Count -and @($coverageSummary | Where-Object { -not $_.passed }).Count -eq 0
    $schemaPassed = $database.database_schema.core -eq $Manifest.candidate.database_schema.core -and $database.database_schema.media -eq $Manifest.candidate.database_schema.media -and $database.database_schema.m3 -eq $Manifest.candidate.database_schema.m3 -and $database.database_schema.m4 -eq $Manifest.candidate.database_schema.m4
    $mediaDurationPassed = [int64]$database.maximum_media_duration_ms -le 600000
    $mediaSamplesPassed = @($mediaSamples | Where-Object { -not $_.passed }).Count -eq 0
    $missingMedia = [int64]0
    foreach ($group in @($database.media_current_by_status | Where-Object { $_.key_1 -in @('missing', 'retention_deleted') })) { $missingMedia += [int64]$group.count }
    $coreLossPassed = $missingMedia -eq 0
    $dayRecords = @(Read-CertificationRecords -Manifest $Manifest | Where-Object {
        $occurred = [DateTime]::Parse([string]$_.occurred_at_utc).ToUniversalTime()
        $occurred -ge $dayStartUTC -and $occurred -lt $dayEndUTC
    })
    $manualInterventions = @($dayRecords | Where-Object { $_.kind -eq 'manual_intervention' })
    $notifications = @($dayRecords | Where-Object { $_.kind -eq 'notification' })
    $publisherRuns = @(Read-MediaPublisherRuns -Manifest $Manifest | Where-Object {
        $started = [DateTime]::Parse([string]$_.started_at_utc).ToUniversalTime()
        $started -ge $dayStartUTC -and $started -lt $dayEndUTC
    })
    $publisherStartUTC = Convert-IsoLocalToUtc -LocalText "$dateText`T$($Manifest.media_publisher.daily_start_local)" -Offset '+08:00'
    $publisherPassed = $publisherRuns.Count -eq 1 -and $publisherRuns[0].status -eq 'passed' -and [int]$publisherRuns[0].accepted_segments -eq [int]$Manifest.media_publisher.segment_count -and [DateTime]::Parse([string]$publisherRuns[0].started_at_utc).ToUniversalTime() -ge $publisherStartUTC -and [DateTime]::Parse([string]$publisherRuns[0].started_at_utc).ToUniversalTime() -lt $publisherStartUTC.AddMinutes(5)
    $crashTimes = @($samples | ForEach-Object { @($_.supervisor.crash_times_utc) } | Where-Object {
        $crash = [DateTime]::Parse([string]$_).ToUniversalTime(); $crash -ge $dayStartUTC -and $crash -lt $dayEndUTC
    } | Sort-Object -Unique)
    $processIDs = @($samples | ForEach-Object { $_.process.pid } | Where-Object { $null -ne $_ } | Sort-Object -Unique)
    $supervisorSummary = [ordered]@{
        crash_count = $crashTimes.Count; process_ids = $processIDs; observed_process_restarts = [Math]::Max(0, $processIDs.Count - 1)
        degraded_samples = @($samples | Where-Object { $_.supervisor.status -eq 'degraded' }).Count
        system_boots = @($samples | ForEach-Object { $_.system_boot_utc } | Sort-Object -Unique)
    }
    $dayPassed = $sampleHealth -and $resourcesPassed -and $duplicatesPassed -and $coveragePassed -and $schemaPassed -and $mediaDurationPassed -and $mediaSamplesPassed -and $publisherPassed -and $backupRPOPassed -and $coreLossPassed -and $manualInterventions.Count -eq 0 -and $database.integrity -eq 'ok'
    $previousDate = $targetDate.AddDays(-1).ToString('yyyy-MM-dd')
    $previousPath = Join-Path $root "daily\$previousDate.json"
    $baseline = if (Test-Path -LiteralPath $previousPath -PathType Leaf) { (Read-JsonFile -Path $previousPath -Code 'M6_PREVIOUS_DAILY_INVALID').database } else { Read-JsonFile -Path (Join-Path $root 'preflight\baseline-database.json') -Code 'M6_BASELINE_DATABASE_INVALID' }
    $countDeltas = [ordered]@{}
    foreach ($property in $database.counts.PSObject.Properties) {
        $old = $baseline.counts.($property.Name)
        $countDeltas[$property.Name] = [int64]$property.Value - [int64]$old
        if ($countDeltas[$property.Name] -lt 0) { $dayPassed = $false }
    }
    $collectorRuntime = @()
    foreach ($pid in @($samples | ForEach-Object { $_.process.pid } | Where-Object { $null -ne $_ } | Sort-Object -Unique)) {
        foreach ($collectorID in @($Manifest.collectors | Where-Object { $_.kind -eq 'activitywatch' } | ForEach-Object { $_.id })) {
            $values = @($samples | Where-Object { $_.process.pid -eq $pid } | ForEach-Object { $_.collectors | Where-Object { $_.collector_id -eq $collectorID } })
            if ($values.Count -gt 0) {
                $collectorRuntime += [ordered]@{ pid = $pid; collector_id = $collectorID; imported = ($values.imported | Measure-Object -Maximum).Maximum; duplicates = ($values.duplicates | Measure-Object -Maximum).Maximum }
            }
        }
    }
    $report = [ordered]@{
        schema_version = 1; date_local = $dateText; generated_at_utc = [DateTime]::UtcNow.ToString('o'); verdict = if ($dayPassed) { 'pass' } else { 'fail' }
        frozen_manifest_sha256 = (Get-Content -Raw -Encoding ASCII -LiteralPath (Join-Path $root 'freeze-manifest.sha256')).Trim()
        sample_count = $samples.Count; maximum_sample_gap_seconds = [int64][Math]::Round($maximumSampleGapSeconds); sample_health_passed = $sampleHealth
        unexpected_unhealthy_samples = $unexpectedUnhealthySamples.Count; resource_peaks = $peak; minimum_free_bytes = $minimumFreeBytes
        backup_storage = $backupMeasurement; resources_passed = $resourcesPassed
        database = $database; schema_passed = $schemaPassed; count_deltas = $countDeltas; collector_runtime_counters = $collectorRuntime; duplicates_passed = $duplicatesPassed
        confirmed_core_loss = [ordered]@{ missing_or_retention_deleted_media = $missingMedia; passed = $coreLossPassed }; supervisor = $supervisorSummary
        backup = $backup; backup_gap_seconds = if ($null -eq $backupGapSeconds) { $null } else { [int64][Math]::Round($backupGapSeconds) }; backup_rpo_passed = $backupRPOPassed
        media_samples = $mediaSamples; media_samples_passed = $mediaSamplesPassed; media_duration_passed = $mediaDurationPassed
        media_publisher_runs = $publisherRuns; media_publisher_passed = $publisherPassed
        coverage = $coverageSummary; coverage_passed = $coveragePassed; excluded_exercise_windows = $excluded
        exercises = @($dayRecords | Where-Object { $_.kind -in $requiredExerciseKinds }); manual_interventions = $manualInterventions; notifications = $notifications
    }
    Write-JsonAtomic -Path $dailyPath -Value $report
    if (-not $dayPassed) { Write-Violation -Manifest $Manifest -Code 'M6_DAILY_GATE_FAILED' -Detail $dateText }
    return $report
}

function Invoke-Record {
    param([Parameter(Mandatory)]$Manifest)
    if (-not $RecordKind -or -not $RecordStatus -or [string]::IsNullOrWhiteSpace($RecordDetail) -or $RecordDetail.Length -gt 1024) { throw 'M6_RECORD_INVALID' }
    Assert-FrozenInputs -Manifest $Manifest
    $now = [DateTime]::UtcNow
    $effectiveStatus = $RecordStatus
    $record = [ordered]@{ schema_version = 1; occurred_at_utc = $now.ToString('o'); kind = $RecordKind; status = $effectiveStatus; detail = $RecordDetail }
    if ($RecordKind -in $requiredExerciseKinds) {
        $exercise = @($Manifest.planned_exercises | Where-Object { $_.kind -eq $RecordKind })
        if ($exercise.Count -ne 1) { throw 'M6_RECORD_EXERCISE_INVALID' }
        $start = [DateTime]::Parse([string]$exercise[0].start_utc).ToUniversalTime()
        $end = [DateTime]::Parse([string]$exercise[0].end_utc).ToUniversalTime()
        if ($RecordStatus -ne 'planned' -and ($now -lt $start -or $now -gt $end)) { throw 'M6_RECORD_OUTSIDE_PLANNED_WINDOW' }
        $record['planned_exercise_id'] = $exercise[0].id
        $record['rto_key'] = $exercise[0].rto_key
        $record['rto_seconds'] = [int]$exercise[0].rto_seconds
        if ($null -ne $RecordDurationSeconds) {
            if ([double]$RecordDurationSeconds -lt 0) { throw 'M6_RECORD_DURATION_INVALID' }
            $record['duration_seconds'] = [double]$RecordDurationSeconds
        }
        if ($RecordStatus -eq 'passed' -and ($null -eq $RecordDurationSeconds -or [double]$RecordDurationSeconds -gt [double]$exercise[0].rto_seconds)) {
            $effectiveStatus = 'failed'; $record['status'] = 'failed'; $record['error_code'] = 'M6_EXERCISE_RTO_MISSED'
        }
    }
    elseif ($RecordKind -eq 'notification') {
        if (-not $RecordSeverity) { throw 'M6_NOTIFICATION_SEVERITY_REQUIRED' }
        $record['severity'] = $RecordSeverity
    }
    Append-JsonLine -Path (Join-Path $Manifest.paths.certification_directory 'records\events.jsonl') -Value $record
    if ($effectiveStatus -eq 'failed' -or $RecordKind -eq 'manual_intervention') { Write-Violation -Manifest $Manifest -Code 'M6_RECORDED_FAILURE' -Detail "$RecordKind`:$effectiveStatus`:$RecordDetail" }
    return $record
}

function Invoke-Finalize {
    param([Parameter(Mandatory)]$Manifest)
    Assert-FrozenInputs -Manifest $Manifest
    Assert-FrozenRepository -Manifest $Manifest
    $now = [DateTime]::UtcNow
    $end = [DateTime]::Parse([string]$Manifest.ends_at_utc).ToUniversalTime()
    if ($now -lt $end) { throw 'M6_FOURTEEN_DAYS_INCOMPLETE' }
    $root = [string]$Manifest.paths.certification_directory
    if (Test-Path -LiteralPath (Join-Path $root 'violations.jsonl')) { throw 'M6_RESET_REQUIRED' }
    $startLocal = [DateTime]::Parse([string]$Manifest.starts_at_utc).ToUniversalTime().AddHours(8).Date
    $daily = @()
    for ($index = 0; $index -lt 14; $index++) {
        $path = Join-Path $root ("daily\" + $startLocal.AddDays($index).ToString('yyyy-MM-dd') + '.json')
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw 'M6_DAILY_REPORT_MISSING' }
        $daily += Read-JsonFile -Path $path -Code 'M6_DAILY_REPORT_INVALID'
    }
    if (@($daily | Where-Object { $_.verdict -ne 'pass' }).Count -ne 0) { throw 'M6_DAILY_GATE_FAILED' }
    $records = @(Read-CertificationRecords -Manifest $Manifest)
    foreach ($kind in $requiredExerciseKinds) {
        $passed = @($records | Where-Object { $_.kind -eq $kind -and $_.status -eq 'passed' -and $null -ne $_.duration_seconds -and [double]$_.duration_seconds -le [double]$_.rto_seconds })
        if ($passed.Count -ne 1) { throw "M6_EXERCISE_MISSING:$kind" }
    }
    if (@($records | Where-Object { $_.kind -eq 'manual_intervention' }).Count -ne 0) { throw 'M6_MANUAL_INTERVENTION_RECORDED' }
    $countTotals = [ordered]@{}
    foreach ($day in $daily) {
        foreach ($property in $day.count_deltas.PSObject.Properties) {
            if (-not $countTotals.Contains($property.Name)) { $countTotals[$property.Name] = [int64]0 }
            $countTotals[$property.Name] += [int64]$property.Value
        }
    }
    $resourcePeaks = [ordered]@{}
    foreach ($day in $daily) {
        foreach ($property in $day.resource_peaks.PSObject.Properties) {
            if ($null -ne $property.Value -and (-not $resourcePeaks.Contains($property.Name) -or [double]$property.Value -gt [double]$resourcePeaks[$property.Name])) { $resourcePeaks[$property.Name] = $property.Value }
        }
    }
    $minimumFree = ($daily | ForEach-Object { $_.minimum_free_bytes } | Measure-Object -Minimum).Minimum
    $maximumBackupBytes = ($daily | ForEach-Object { $_.backup_storage.bytes } | Measure-Object -Maximum).Maximum
    $maximumBackupGap = ($daily | ForEach-Object { $_.backup_gap_seconds } | Measure-Object -Maximum).Maximum
    $crashCount = ($daily | ForEach-Object { $_.supervisor.crash_count } | Measure-Object -Sum).Sum
    $restartCount = ($daily | ForEach-Object { $_.supervisor.observed_process_restarts } | Measure-Object -Sum).Sum
    $lastDatabase = $daily[-1].database
    $report = [ordered]@{
        schema_version = 1; certification_id = $Manifest.certification_id; verdict = 'pass'; finalized_at_utc = $now.ToString('o')
        frozen_manifest_sha256 = (Get-Content -Raw -Encoding ASCII -LiteralPath (Join-Path $root 'freeze-manifest.sha256')).Trim()
        candidate = $Manifest.candidate; starts_at_utc = $Manifest.starts_at_utc; ends_at_utc = $Manifest.ends_at_utc
        previous_stable = $Manifest.previous_stable; configuration = $Manifest.configuration; dependencies = $Manifest.dependencies
        limits = $Manifest.limits; recovery_rto_seconds = $Manifest.recovery_rto_seconds; backup_rpo_hours = $Manifest.backup.rpo_hours
        consecutive_days = 14
        daily_reports = @($daily | ForEach-Object {
            [ordered]@{
                date_local = $_.date_local; verdict = $_.verdict; sample_count = $_.sample_count; maximum_sample_gap_seconds = $_.maximum_sample_gap_seconds
                coverage = $_.coverage; database_integrity = $_.database.integrity; backup_gap_seconds = $_.backup_gap_seconds
                media_samples_passed = $_.media_samples_passed; media_publisher_passed = $_.media_publisher_passed; resources_passed = $_.resources_passed
            }
        })
        evidence_count_deltas = $countTotals
        confirmed_core_loss = [ordered]@{ raw_events = 0; accepted_media = 0 }
        potential_duplicate_groups = $lastDatabase.potential_duplicates
        media = [ordered]@{ maximum_duration_ms = $lastDatabase.maximum_media_duration_ms; final_current_status = $lastDatabase.media_current_by_status; final_ingest_status = $lastDatabase.media_ingest_by_status }
        operations = [ordered]@{
            crash_count = $crashCount; observed_process_restarts = $restartCount; resource_peaks = $resourcePeaks
            minimum_free_bytes = $minimumFree; maximum_backup_storage_bytes = $maximumBackupBytes; maximum_backup_gap_seconds = $maximumBackupGap
        }
        record_only_available = $true; manual_intervention_count = 0
        fault_and_recovery_records = @($records | Where-Object { $_.kind -in $requiredExerciseKinds })
        notifications = @($records | Where-Object { $_.kind -eq 'notification' })
        remaining_risks = @('long-term media retention remains disabled and requires a future separately frozen policy')
    }
    Write-JsonAtomic -Path (Join-Path $root 'final-report.json') -Value $report
    return $report
}

if ($MyInvocation.InvocationName -eq '.') { return }

try {
    if ($Action -eq 'Initialize') {
        $manifest = Invoke-Initialize
        Invoke-InstallTasks -Manifest $manifest
        $manifest | ConvertTo-Json -Depth 32
        exit 0
    }
    if (-not $CertificationDirectory) { throw 'M6_CERTIFICATION_DIRECTORY_REQUIRED' }
    $manifest = Read-SealedManifest -Root $CertificationDirectory
    switch ($Action) {
        'InstallTasks' { Invoke-InstallTasks -Manifest $manifest; [ordered]@{ status = 'installed'; tasks = $manifest.tasks } | ConvertTo-Json -Depth 8 }
        'RemoveTasks' { Invoke-RemoveTasks -Manifest $manifest; [ordered]@{ status = 'removed'; tasks = $manifest.tasks } | ConvertTo-Json -Depth 8 }
        'Sample' { Invoke-Sample -Manifest $manifest | ConvertTo-Json -Depth 32 }
        'Backup' { Invoke-CertificationBackup -Manifest $manifest | ConvertTo-Json -Depth 16 }
        'Daily' { Invoke-Daily -Manifest $manifest -RequestedDate $Date | ConvertTo-Json -Depth 32 }
        'Record' { Invoke-Record -Manifest $manifest | ConvertTo-Json -Depth 16 }
        'Finalize' { Invoke-Finalize -Manifest $manifest | ConvertTo-Json -Depth 32 }
    }
}
catch {
    if ($CertificationDirectory -and (Test-Path -LiteralPath (Join-Path $CertificationDirectory 'freeze-manifest.json'))) {
        try {
            $manifest = Read-SealedManifest -Root $CertificationDirectory
            $code = if ($_.Exception.Message -match '^([A-Z0-9_]+)') { $Matches[1] } else { 'M6_CERTIFICATION_FAILED' }
            if ($code -in @('M6_CANDIDATE_BINARY_CHANGED', 'M6_RELEASE_MANIFEST_CHANGED', 'M6_CONFIG_CHANGED', 'M6_CERTIFICATION_TOOL_CHANGED', 'M6_MEDIA_PUBLISHER_TOOL_CHANGED', 'M6_CURRENT_POINTER_INVALID', 'M6_ACTIVE_RELEASE_CHANGED', 'M6_REPOSITORY_CHANGED', 'M6_DEPENDENCY_CHANGED')) {
                Write-Violation -Manifest $manifest -Code $code -Detail $_.Exception.Message
            }
        }
        catch { }
    }
    throw
}
