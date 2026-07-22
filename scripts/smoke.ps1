[CmdletBinding()]
param([string]$BinaryPath)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor m3-smoke-" + [guid]::NewGuid().ToString('N'))
$buildDirectory = Join-Path $temporaryRoot 'bin'
$configPath = Join-Path $temporaryRoot 'config.json'
$dataDirectory = Join-Path $temporaryRoot 'data'
$mediaInbox = Join-Path $temporaryRoot 'media-inbox'
$validMediaPath = Join-Path $mediaInbox 'valid.mp4'
$validSidecarPath = $validMediaPath + '.sidecar.json'
$validReadyPath = $validMediaPath + '.ready'
$validConfirmationPath = $validMediaPath + '.accepted.json'
$corruptMediaPath = Join-Path $mediaInbox 'corrupt.mp4'
$corruptSidecarPath = $corruptMediaPath + '.sidecar.json'
$corruptReadyPath = $corruptMediaPath + '.ready'
$corruptConfirmationPath = $corruptMediaPath + '.accepted.json'
$firstStdout = Join-Path $temporaryRoot 'first-stdout.log'
$firstStderr = Join-Path $temporaryRoot 'first-stderr.log'
$secondStdout = Join-Path $temporaryRoot 'second-stdout.log'
$secondStderr = Join-Path $temporaryRoot 'second-stderr.log'
$process = $null

function Get-FreeLoopbackPort {
    $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Write-Utf8WithoutBOM {
    param([string]$Path, [string]$Contents)

    [IO.File]::WriteAllText($Path, $Contents, (New-Object Text.UTF8Encoding($false)))
}

function Start-MonitorProcess {
    param(
        [string]$BinaryPath,
        [string]$ConfigurationPath,
        [string]$StandardOutput,
        [string]$StandardError,
        [string]$RunFor
    )

    $configArgument = '--config="' + $ConfigurationPath + '"'
    return Start-Process -FilePath $BinaryPath `
        -ArgumentList @($configArgument, "--run-for=$RunFor") `
        -RedirectStandardOutput $StandardOutput `
        -RedirectStandardError $StandardError `
        -WindowStyle Hidden `
        -PassThru
}

function Wait-ForReadiness {
    param([Diagnostics.Process]$MonitorProcess, [string]$BaseURL)

    $deadline = [DateTime]::UtcNow.AddSeconds(5)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($MonitorProcess.HasExited) {
            break
        }
        try {
            $response = Invoke-RestMethod -Uri "$BaseURL/health/ready" -Method Get -TimeoutSec 1
            if ($response.status -eq 'writable' -and $response.schema_version -eq 1) {
                return $response
            }
        }
        catch {
            Start-Sleep -Milliseconds 100
        }
    }
    throw 'writable readiness endpoint did not become available'
}

function Wait-ForMediaStatus {
    param(
        [Diagnostics.Process]$MonitorProcess,
        [string]$BaseURL,
        [scriptblock]$Predicate,
        [string]$Description
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(8)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($MonitorProcess.HasExited) {
            break
        }
        try {
            $status = Invoke-RestMethod -Uri "$BaseURL/api/v1/media/ingest/status" -Method Get -TimeoutSec 1
            if (& $Predicate $status) {
                return $status
            }
        }
        catch {
        }
        Start-Sleep -Milliseconds 100
    }
    throw "media ingest did not reach $Description"
}

function Write-MediaSidecar {
    param(
        [string]$Path,
        [long]$SizeBytes,
        [string]$SHA256,
        [string]$SourceKey
    )

    $sidecar = [ordered]@{
        schema_version = 1
        complete = $true
        collector_id = 'smoke.ffmpeg'
        source_idempotency_key = $SourceKey
        device_start_raw = '2026-07-19T10:00:00+08:00'
        device_end_raw = '2026-07-19T10:00:01+08:00'
        clock_offset_ms = 0
        clock_error_ms = 25
        size_bytes = $SizeBytes
        sha256 = $SHA256
        media_type = 'video'
    }
    Write-Utf8WithoutBOM -Path $Path -Contents ($sidecar | ConvertTo-Json -Depth 4)
}

function Invoke-EventBatch {
    param([string]$BaseURL, [object[]]$Events)

    $body = [ordered]@{ schema_version = 1; events = $Events } | ConvertTo-Json -Depth 10
    return Invoke-RestMethod -Uri "$BaseURL/api/v1/events/batch" -Method Post -ContentType 'application/json' -Body $body -TimeoutSec 3
}

function Invoke-HeartbeatBatch {
    param([string]$BaseURL, [object[]]$Heartbeats)

    $body = [ordered]@{ schema_version = 1; heartbeats = $Heartbeats } | ConvertTo-Json -Depth 10
    return Invoke-RestMethod -Uri "$BaseURL/api/v1/collectors/heartbeats/batch" -Method Post -ContentType 'application/json' -Body $body -TimeoutSec 3
}

function Assert-CleanProcessStop {
    param([Diagnostics.Process]$MonitorProcess, [string]$StandardError)

    if (-not $MonitorProcess.WaitForExit(35000)) {
        throw 'exam-monitor did not exit within the smoke timeout'
    }
    $records = @()
    foreach ($line in @(Get-Content -LiteralPath $StandardError -Encoding UTF8)) {
        if ($line.Trim()) {
            $records += $line | ConvertFrom-Json
        }
    }
    if (-not ($records.event -contains 'started') -or -not ($records.event -contains 'stopped')) {
        throw 'structured logs do not contain both started and stopped events'
    }
    foreach ($record in $records) {
        foreach ($field in @('time', 'level', 'service', 'build_version', 'component', 'event')) {
            if (-not $record.PSObject.Properties[$field]) {
                throw "structured log missing $field"
            }
        }
    }
}

try {
    [void](New-Item -ItemType Directory -Path $temporaryRoot)
    [void](New-Item -ItemType Directory -Path $mediaInbox)
    $ffprobeCommand = Get-Command ffprobe -CommandType Application -ErrorAction Stop | Select-Object -First 1
    $ffprobePath = $ffprobeCommand.Source
    $ffprobeVersionLine = (& $ffprobePath -version | Select-Object -First 1)
    $expectedFFprobeVersion = 'N-117599-ge1d1ba4cbc-20241017'
    if ($ffprobeVersionLine -notmatch '^ffprobe version ([^ ]+) ' -or $Matches[1] -ne $expectedFFprobeVersion) {
        throw "M2 requires ffprobe $expectedFFprobeVersion; found: $ffprobeVersionLine"
    }

    $fixtureEncoded = (Get-Content -LiteralPath (Join-Path $repoRoot 'testdata\media\valid.mp4.b64') -Raw -Encoding UTF8).Trim()
    $fixtureBytes = [Convert]::FromBase64String($fixtureEncoded)
    [IO.File]::WriteAllBytes($validMediaPath, $fixtureBytes)
    $fixtureSHA256 = (Get-FileHash -LiteralPath $validMediaPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($fixtureBytes.Length -ne 2195 -or $fixtureSHA256 -ne '346f4da339e0f6f91e1785436c74f97a42b87392e6f4939b2fc01b5e12f442d8') {
        throw 'M2 fixed media fixture size or SHA-256 changed'
    }
    Write-MediaSidecar -Path $validSidecarPath -SizeBytes $fixtureBytes.Length -SHA256 $fixtureSHA256 -SourceKey 'm2-smoke-valid-1'
    [IO.File]::WriteAllBytes($validReadyPath, [byte[]]@())

    if ($BinaryPath) {
        if (-not [IO.Path]::IsPathRooted($BinaryPath) -or -not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) {
            throw 'smoke candidate binary is invalid'
        }
        $binaryPath = [IO.Path]::GetFullPath($BinaryPath)
    }
    else {
        $binaryPath = ((& (Join-Path $PSScriptRoot 'build.ps1') -OutputDirectory $buildDirectory) | Select-Object -Last 1)
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) {
            throw 'smoke build did not produce exam-monitor.exe'
        }
    }

    $port = Get-FreeLoopbackPort
    $baseURL = "http://127.0.0.1:$port"
    $config = [ordered]@{
        schema_version = 1
        runtime = [ordered]@{ mode = 'record-only'; backup_interface_enabled = $true }
        server = [ordered]@{
            listen_address = "127.0.0.1:$port"
            allow_non_loopback = $false
            read_header_timeout = '2s'
            read_timeout = '3s'
            write_timeout = '3s'
            idle_timeout = '5s'
            shutdown_timeout = '2s'
        }
        paths = [ordered]@{ data_directory = $dataDirectory }
        storage = [ordered]@{ busy_timeout = '1s'; max_open_connections = 4 }
        api = [ordered]@{
            max_request_bytes = 1048576
            max_batch_events = 100
            max_event_bytes = 65536
            max_payload_depth = 16
            max_concurrent_writes = 4
            default_page_size = 100
            max_page_size = 500
        }
        media_ingest = [ordered]@{
            enabled = $true
            inbox_directory = $mediaInbox
            scan_interval = '100ms'
            settle_interval = '100ms'
            max_segment_bytes = 1048576
            max_segment_duration = '10m'
            max_sidecar_bytes = 65536
            max_scan_entries = 1000
            ffprobe_path = $ffprobePath
            ffprobe_timeout = '5s'
        }
        collectors = @(
            [ordered]@{
                id = 'smoke.desktop'
                kind = 'generic_json'
                enabled = $true
                heartbeat_period = '1m'
                allowed_lateness = '1m'
                offline_after = '5m'
                planned_schedule = [ordered]@{
                    timezone = 'UTC'
                    windows = @([ordered]@{ days = @('monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'); start_local = '00:00'; end_local = '24:00' })
                }
            },
            [ordered]@{
                id = 'smoke.ffmpeg'
                kind = 'media'
                enabled = $true
                heartbeat_period = '5m'
                allowed_lateness = '5m'
                offline_after = '15m'
                planned_schedule = [ordered]@{
                    timezone = 'UTC'
                    windows = @([ordered]@{ days = @('monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'); start_local = '00:00'; end_local = '24:00' })
                }
            },
            [ordered]@{
                id = 'smoke.offline'
                kind = 'generic_json'
                enabled = $true
                heartbeat_period = '1m'
                allowed_lateness = '1m'
                offline_after = '5m'
                planned_schedule = [ordered]@{
                    timezone = 'UTC'
                    windows = @([ordered]@{ days = @('monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'); start_local = '00:00'; end_local = '24:00' })
                }
            }
        )
        timeline = [ordered]@{ clock_uncertain_after = '1s'; max_query_range = '744h'; max_projection_facts = 100000 }
        logging = [ordered]@{ level = 'info' }
    }
    Write-Utf8WithoutBOM -Path $configPath -Contents ($config | ConvertTo-Json -Depth 6)

    $checkJSON = ((& $binaryPath "--config=$configPath" --check-config) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "--check-config failed with exit code $LASTEXITCODE"
    }
    $check = $checkJSON | ConvertFrom-Json
    $expectedDatabase = Join-Path $dataDirectory 'exam-monitor.db'
    if ($check.status -ne 'ok' -or $check.data_directory -ne $dataDirectory -or $check.database_path -ne $expectedDatabase -or -not $check.media_ingest_enabled -or -not $check.dashboard_enabled -or $check.media_inbox_directory -ne $mediaInbox -or $check.ffprobe_path -ne $ffprobePath -or $check.mode -ne 'record-only' -or $check.enabled_collectors -ne 3) {
        throw "unexpected --check-config output: $checkJSON"
    }
    if (Test-Path -LiteralPath $dataDirectory) {
        throw '--check-config created the runtime data directory'
    }

    $process = Start-MonitorProcess -BinaryPath $binaryPath -ConfigurationPath $configPath -StandardOutput $firstStdout -StandardError $firstStderr -RunFor '25s'
    [void](Wait-ForReadiness -MonitorProcess $process -BaseURL $baseURL)

    $dashboardPage = Invoke-WebRequest -UseBasicParsing -Uri "$baseURL/" -Method Get -TimeoutSec 3
    foreach ($panel in @('storage', 'collectors', 'backlog', 'coverage', 'timeline', 'faults')) {
        if ($dashboardPage.Content -notmatch ('data-panel="' + $panel + '"')) {
            throw "embedded dashboard is missing the $panel panel"
        }
    }
    if ($dashboardPage.Content -match '(?i)<form') {
        throw 'read-only dashboard unexpectedly contains a form'
    }
    $dashboardAsset = Invoke-WebRequest -UseBasicParsing -Uri "$baseURL/assets/app.js" -Method Get -TimeoutSec 3
    if ($dashboardAsset.Content -notmatch '/api/v1/dashboard/summary' -or $dashboardAsset.Content -match '(?i)method:\s*["'']POST') {
        throw 'embedded dashboard asset does not preserve the GET-only summary contract'
    }
    $dashboardSummary = Invoke-RestMethod -Uri "$baseURL/api/v1/dashboard/summary" -Method Get -TimeoutSec 3
    if ($dashboardSummary.schema_version -ne 1 -or $dashboardSummary.runtime_mode -ne 'record-only' -or $dashboardSummary.analysis.status -ne 'not_installed' -or $null -ne $dashboardSummary.analysis.backlog -or $dashboardSummary.external_backlogs.Count -ne 3) {
        throw "dashboard absence/backlog contract is inaccurate: $($dashboardSummary | ConvertTo-Json -Depth 8 -Compress)"
    }
    foreach ($backlog in $dashboardSummary.external_backlogs) {
        if ($backlog.status -ne 'unknown' -or $null -ne $backlog.items -or $null -ne $backlog.bytes) {
            throw "unreported external backlog became a confirmed zero: $($backlog | ConvertTo-Json -Compress)"
        }
    }

    $event = [ordered]@{
        schema_version = 1
        collector_id = 'smoke.desktop'
        event_type = 'study.activity'
        device_timestamp_raw = '2026-07-18T10:00:00+08:00'
        clock_offset_ms = 125
        clock_error_ms = 50
        idempotency_key = 'm1-smoke-event-1'
        payload = [ordered]@{ window = 'notes'; seconds = 42 }
    }
    $accepted = Invoke-EventBatch -BaseURL $baseURL -Events @($event)
    if ($accepted.schema_version -ne 1 -or $accepted.results[0].status -ne 'accepted' -or $accepted.results[0].event_id -le 0) {
        throw "unexpected accepted response: $($accepted | ConvertTo-Json -Depth 6 -Compress)"
    }
    $eventID = $accepted.results[0].event_id

    $evidenceAlias = Invoke-RestMethod -Uri "$baseURL/api/v1/evidence/batch" -Method Post -ContentType 'application/json' -Body ([ordered]@{ schema_version = 1; events = @($event) } | ConvertTo-Json -Depth 10) -TimeoutSec 3
    if ($evidenceAlias.results[0].status -ne 'duplicate' -or $evidenceAlias.results[0].event_id -ne $eventID) {
        throw "generic Evidence alias did not preserve M1 idempotency: $($evidenceAlias | ConvertTo-Json -Depth 6 -Compress)"
    }

    $heartbeat = [ordered]@{
        schema_version = 1
        collector_id = 'smoke.desktop'
        state = 'idle'
        device_start_raw = '2026-07-18T10:00:00+08:00'
        device_end_raw = '2026-07-18T10:01:00+08:00'
        clock_offset_ms = 0
        clock_error_ms = 25
        idempotency_key = 'm3-smoke-heartbeat-1'
        quality_flags = @()
    }
    $heartbeatAccepted = Invoke-HeartbeatBatch -BaseURL $baseURL -Heartbeats @($heartbeat)
    $heartbeatDuplicate = Invoke-HeartbeatBatch -BaseURL $baseURL -Heartbeats @($heartbeat)
    if ($heartbeatAccepted.results[0].status -ne 'accepted' -or $heartbeatDuplicate.results[0].status -ne 'duplicate' -or $heartbeatAccepted.results[0].heartbeat_id -ne $heartbeatDuplicate.results[0].heartbeat_id) {
        throw 'heartbeat append/replay did not preserve idempotency'
    }

    $duplicate = Invoke-EventBatch -BaseURL $baseURL -Events @($event)
    if ($duplicate.results[0].status -ne 'duplicate' -or $duplicate.results[0].event_id -ne $eventID) {
        throw "unexpected duplicate response: $($duplicate | ConvertTo-Json -Depth 6 -Compress)"
    }

    $conflictingEvent = [ordered]@{
        schema_version = 1
        collector_id = 'smoke.desktop'
        event_type = 'study.activity'
        device_timestamp_raw = '2026-07-18T10:00:00+08:00'
        clock_offset_ms = 125
        clock_error_ms = 50
        idempotency_key = 'm1-smoke-event-1'
        payload = [ordered]@{ window = 'different'; seconds = 42 }
    }
    $conflict = Invoke-EventBatch -BaseURL $baseURL -Events @($conflictingEvent)
    if ($conflict.results[0].status -ne 'conflict' -or $conflict.results[0].event_id -ne $eventID) {
        throw "unexpected conflict response: $($conflict | ConvertTo-Json -Depth 6 -Compress)"
    }

    $query = Invoke-RestMethod -Uri "$baseURL/api/v1/events?limit=10" -Method Get -TimeoutSec 3
    if ($query.schema_version -ne 1 -or $query.events.Count -ne 1 -or $query.events[0].id -ne $eventID -or $query.events[0].payload.window -ne 'notes') {
        throw "unexpected event query: $($query | ConvertTo-Json -Depth 8 -Compress)"
    }

    $mediaAccepted = Wait-ForMediaStatus -MonitorProcess $process -BaseURL $baseURL -Description 'one accepted segment' -Predicate {
        param($status)
        $status.status -eq 'healthy' -and
            $status.ffprobe_version -eq $expectedFFprobeVersion -and
            $status.ingest.accepted -eq 1 -and
            $status.ingest.total_segments -eq 1 -and
            $status.ingest.backlog -eq 0 -and
            (Test-Path -LiteralPath $validConfirmationPath -PathType Leaf)
    }
    if (-not (Test-Path -LiteralPath $validConfirmationPath -PathType Leaf)) {
        throw 'accepted media confirmation marker was not created'
    }
    if (-not (Test-Path -LiteralPath $validMediaPath -PathType Leaf)) {
        throw 'Recorder Core removed the source media before collector confirmation'
    }
    $firstConfirmation = Get-Content -LiteralPath $validConfirmationPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($firstConfirmation.media_segment_id -le 0 -or $firstConfirmation.sha256 -ne $fixtureSHA256) {
        throw 'accepted media confirmation has unexpected identity or checksum'
    }
    $acceptedMediaPath = Join-Path $dataDirectory ("media\accepted\$fixtureSHA256.media")
    if (-not (Test-Path -LiteralPath $acceptedMediaPath -PathType Leaf)) {
        throw 'accepted media is missing from Recorder Core managed storage'
    }
    if ((Get-FileHash -LiteralPath $acceptedMediaPath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $fixtureSHA256) {
        throw 'managed accepted media checksum does not match the sidecar'
    }

    $timelineStart = [Uri]::EscapeDataString('2026-07-18T00:00:00Z')
    $timelineEnd = [Uri]::EscapeDataString('2026-07-20T23:59:00Z')
    $timeline = Invoke-RestMethod -Uri "$baseURL/api/v1/timeline?start=$timelineStart&end=$timelineEnd&limit=100" -Method Get -TimeoutSec 5
    $sourceTypes = @($timeline.entries | ForEach-Object { $_.source_type } | Sort-Object -Unique)
    if ($timeline.schema_version -ne 1 -or $timeline.entries.Count -lt 3 -or -not ($sourceTypes -contains 'raw_event') -or -not ($sourceTypes -contains 'heartbeat') -or -not ($sourceTypes -contains 'media_segment')) {
        throw "multi-source timeline is incomplete: $($timeline | ConvertTo-Json -Depth 8 -Compress)"
    }
    foreach ($entry in $timeline.entries) {
        foreach ($field in @('stable_id', 'device_start_raw', 'device_start_utc', 'received_at_utc', 'corrected_start_utc', 'clock_error_ms', 'clock_uncertain')) {
            if (-not $entry.PSObject.Properties[$field]) {
                throw "timeline entry missing $field"
            }
        }
    }

    $coverageStart = [Uri]::EscapeDataString('2026-07-18T01:55:00Z')
    $coverageEnd = [Uri]::EscapeDataString('2026-07-18T02:10:00Z')
    $offlineCoverage = Invoke-RestMethod -Uri "$baseURL/api/v1/coverage?start=$coverageStart&end=$coverageEnd&collector_id=smoke.offline" -Method Get -TimeoutSec 5
    $offlineIntervals = @($offlineCoverage.intervals)
    if ($offlineCoverage.projections.Count -ne 1 -or $offlineCoverage.projections[0].status -ne 'fresh' -or -not ($offlineIntervals.availability -contains 'offline') -or ($offlineIntervals.availability -contains 'confirmed_idle')) {
        throw "offline collector gap was not exposed accurately: $($offlineCoverage | ConvertTo-Json -Depth 8 -Compress)"
    }
    for ($index = 1; $index -lt $offlineIntervals.Count; $index++) {
        if ($offlineIntervals[$index].start_utc -lt $offlineIntervals[$index - 1].end_utc) {
            throw 'coverage projection contains overlapping intervals'
        }
    }

    $populatedDashboard = Invoke-RestMethod -Uri "$baseURL/api/v1/dashboard/summary" -Method Get -TimeoutSec 3
    if ($populatedDashboard.media.ingest.accepted -ne 1 -or $populatedDashboard.media.ingest.backlog -ne 0 -or $populatedDashboard.history.modules.Count -lt 6 -or $populatedDashboard.external_backlogs.Count -ne 3) {
        throw "dashboard did not expose bounded Recorder Core state: $($populatedDashboard | ConvertTo-Json -Depth 8 -Compress)"
    }

    Remove-Item -LiteralPath $validConfirmationPath -Force
    [void](Wait-ForMediaStatus -MonitorProcess $process -BaseURL $baseURL -Description 'idempotent confirmation replay' -Predicate {
        param($status)
        (Test-Path -LiteralPath $validConfirmationPath -PathType Leaf) -and
            $status.ingest.accepted -eq 1 -and
            $status.ingest.total_segments -eq 1
    })
    $replayedConfirmation = Get-Content -LiteralPath $validConfirmationPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($replayedConfirmation.media_segment_id -ne $firstConfirmation.media_segment_id) {
        throw 'media replay returned a different media segment id'
    }

    Copy-Item -LiteralPath $validMediaPath -Destination $corruptMediaPath
    Write-MediaSidecar -Path $corruptSidecarPath -SizeBytes $fixtureBytes.Length -SHA256 ('0' * 64) -SourceKey 'm2-smoke-corrupt-1'
    [IO.File]::WriteAllBytes($corruptReadyPath, [byte[]]@())
    $mediaQuarantined = Wait-ForMediaStatus -MonitorProcess $process -BaseURL $baseURL -Description 'checksum mismatch quarantine' -Predicate {
        param($status)
        $status.ingest.quarantined -eq 1 -and
            $status.ingest.total_segments -eq 1 -and
            $status.ingest.last_error_code -eq 'MEDIA_HASH_MISMATCH'
    }
    if (Test-Path -LiteralPath $corruptConfirmationPath) {
        throw 'quarantined media received an accepted confirmation'
    }
    if (-not (Test-Path -LiteralPath $corruptMediaPath -PathType Leaf)) {
        throw 'quarantine removed the unconfirmed source media'
    }
    $reasonFiles = @(Get-ChildItem -LiteralPath (Join-Path $dataDirectory 'media\quarantine') -Filter '*.reason.json' -File)
    if ($reasonFiles.Count -ne 1) {
        throw "expected one managed quarantine reason, found $($reasonFiles.Count)"
    }
    $reason = Get-Content -LiteralPath $reasonFiles[0].FullName -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($reason.reason_code -ne 'MEDIA_HASH_MISMATCH') {
        throw "unexpected quarantine reason: $($reason.reason_code)"
    }
    Assert-CleanProcessStop -MonitorProcess $process -StandardError $firstStderr
    $process = $null

    $process = Start-MonitorProcess -BinaryPath $binaryPath -ConfigurationPath $configPath -StandardOutput $secondStdout -StandardError $secondStderr -RunFor '12s'
    [void](Wait-ForReadiness -MonitorProcess $process -BaseURL $baseURL)
    $restartQuery = Invoke-RestMethod -Uri "$baseURL/api/v1/events?limit=10" -Method Get -TimeoutSec 3
    if ($restartQuery.events.Count -ne 1 -or $restartQuery.events[0].id -ne $eventID -or $restartQuery.events[0].idempotency_key -ne 'm1-smoke-event-1') {
        throw "confirmed event missing after restart: $($restartQuery | ConvertTo-Json -Depth 8 -Compress)"
    }
    $restartMedia = Wait-ForMediaStatus -MonitorProcess $process -BaseURL $baseURL -Description 'persisted media state after restart' -Predicate {
        param($status)
        $status.status -eq 'healthy' -and
            $status.ingest.accepted -eq 1 -and
            $status.ingest.quarantined -eq 1 -and
            $status.ingest.total_segments -eq 1
    }
    $restartConfirmation = Get-Content -LiteralPath $validConfirmationPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($restartConfirmation.media_segment_id -ne $firstConfirmation.media_segment_id -or
        -not (Test-Path -LiteralPath $acceptedMediaPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $validMediaPath -PathType Leaf) -or
        -not (Test-Path -LiteralPath $corruptMediaPath -PathType Leaf)) {
        throw 'media acceptance, confirmation, quarantine, or source preservation did not survive restart'
    }
    Assert-CleanProcessStop -MonitorProcess $process -StandardError $secondStderr
    $process = $null

    if (-not (Test-Path -LiteralPath $expectedDatabase -PathType Leaf)) {
        throw 'M2 smoke database was not created in the temporary data directory'
    }
    & (Join-Path $PSScriptRoot 'smoke-m4.ps1') -BinaryPath $binaryPath -SourceConfigPath $configPath -SourceDataDirectory $dataDirectory
    Write-Output "M5 smoke passed over M1-M4 compatibility: embedded read-only dashboard, explicit unknown/zero states, generic Evidence, media recovery, unified timeline, coverage, and frozen operations ($baseURL)"
}
catch {
    foreach ($logPath in @($firstStderr, $secondStderr)) {
        if (Test-Path -LiteralPath $logPath) {
            Write-Warning ((Get-Content -Raw -LiteralPath $logPath -Encoding UTF8).Trim())
        }
    }
    throw
}
finally {
    if ($null -ne $process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        [void]$process.WaitForExit(5000)
    }
    for ($attempt = 0; $attempt -lt 3 -and (Test-Path -LiteralPath $temporaryRoot); $attempt++) {
        try {
            Remove-Item -LiteralPath $temporaryRoot -Recurse -Force -ErrorAction Stop
        }
        catch {
            if ($attempt -eq 2) {
                throw
            }
            Start-Sleep -Milliseconds 100
        }
    }
}
