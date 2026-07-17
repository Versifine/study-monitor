[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor m1-smoke-" + [guid]::NewGuid().ToString('N'))
$buildDirectory = Join-Path $temporaryRoot 'bin'
$configPath = Join-Path $temporaryRoot 'config.json'
$dataDirectory = Join-Path $temporaryRoot 'data'
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

function Invoke-EventBatch {
    param([string]$BaseURL, [object[]]$Events)

    $body = [ordered]@{ schema_version = 1; events = $Events } | ConvertTo-Json -Depth 10
    return Invoke-RestMethod -Uri "$BaseURL/api/v1/events/batch" -Method Post -ContentType 'application/json' -Body $body -TimeoutSec 3
}

function Assert-CleanProcessStop {
    param([Diagnostics.Process]$MonitorProcess, [string]$StandardError)

    if (-not $MonitorProcess.WaitForExit(15000)) {
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
    $binaryPath = ((& (Join-Path $PSScriptRoot 'build.ps1') -OutputDirectory $buildDirectory) | Select-Object -Last 1)
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) {
        throw 'smoke build did not produce exam-monitor.exe'
    }

    $port = Get-FreeLoopbackPort
    $baseURL = "http://127.0.0.1:$port"
    $config = [ordered]@{
        schema_version = 1
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
        logging = [ordered]@{ level = 'info' }
    }
    Write-Utf8WithoutBOM -Path $configPath -Contents ($config | ConvertTo-Json -Depth 6)

    $checkJSON = ((& $binaryPath "--config=$configPath" --check-config) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "--check-config failed with exit code $LASTEXITCODE"
    }
    $check = $checkJSON | ConvertFrom-Json
    $expectedDatabase = Join-Path $dataDirectory 'exam-monitor.db'
    if ($check.status -ne 'ok' -or $check.data_directory -ne $dataDirectory -or $check.database_path -ne $expectedDatabase) {
        throw "unexpected --check-config output: $checkJSON"
    }
    if (Test-Path -LiteralPath $dataDirectory) {
        throw '--check-config created the runtime data directory'
    }

    $process = Start-MonitorProcess -BinaryPath $binaryPath -ConfigurationPath $configPath -StandardOutput $firstStdout -StandardError $firstStderr -RunFor '8s'
    [void](Wait-ForReadiness -MonitorProcess $process -BaseURL $baseURL)

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
    Assert-CleanProcessStop -MonitorProcess $process -StandardError $firstStderr
    $process = $null

    $process = Start-MonitorProcess -BinaryPath $binaryPath -ConfigurationPath $configPath -StandardOutput $secondStdout -StandardError $secondStderr -RunFor '4s'
    [void](Wait-ForReadiness -MonitorProcess $process -BaseURL $baseURL)
    $restartQuery = Invoke-RestMethod -Uri "$baseURL/api/v1/events?limit=10" -Method Get -TimeoutSec 3
    if ($restartQuery.events.Count -ne 1 -or $restartQuery.events[0].id -ne $eventID -or $restartQuery.events[0].idempotency_key -ne 'm1-smoke-event-1') {
        throw "confirmed event missing after restart: $($restartQuery | ConvertTo-Json -Depth 8 -Compress)"
    }
    Assert-CleanProcessStop -MonitorProcess $process -StandardError $secondStderr
    $process = $null

    if (-not (Test-Path -LiteralPath $expectedDatabase -PathType Leaf)) {
        throw 'M1 smoke database was not created in the temporary data directory'
    }
    Write-Output "M1 smoke passed: write, replay, conflict, query, restart query ($baseURL)"
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
    if (Test-Path -LiteralPath $temporaryRoot) {
        Remove-Item -LiteralPath $temporaryRoot -Recurse -Force
    }
}
