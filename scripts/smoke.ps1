[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$temporaryRoot = Join-Path ([IO.Path]::GetTempPath()) ("exam-monitor smoke-" + [guid]::NewGuid().ToString('N'))
$buildDirectory = Join-Path $temporaryRoot 'bin'
$configPath = Join-Path $temporaryRoot 'config.json'
$stdoutPath = Join-Path $temporaryRoot 'stdout.log'
$stderrPath = Join-Path $temporaryRoot 'stderr.log'
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

try {
    [void](New-Item -ItemType Directory -Path $temporaryRoot)
    $binaryPath = ((& (Join-Path $PSScriptRoot 'build.ps1') -OutputDirectory $buildDirectory) | Select-Object -Last 1)
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $binaryPath -PathType Leaf)) {
        throw 'smoke build did not produce exam-monitor.exe'
    }

    $port = Get-FreeLoopbackPort
    $config = [ordered]@{
        schema_version = 1
        server = [ordered]@{
            listen_address = "127.0.0.1:$port"
            allow_non_loopback = $false
            read_header_timeout = '2s'
            shutdown_timeout = '2s'
        }
        paths = [ordered]@{ data_directory = (Join-Path $temporaryRoot 'data') }
        logging = [ordered]@{ level = 'info' }
    }
    Write-Utf8WithoutBOM -Path $configPath -Contents ($config | ConvertTo-Json -Depth 4)

    $checkJSON = ((& $binaryPath "--config=$configPath" --check-config) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "--check-config failed with exit code $LASTEXITCODE"
    }
    $check = $checkJSON | ConvertFrom-Json
    if ($check.status -ne 'ok' -or $check.listen_address -ne "127.0.0.1:$port" -or $check.data_directory -ne (Join-Path $temporaryRoot 'data')) {
        throw "unexpected --check-config output: $checkJSON"
    }
    if (Test-Path -LiteralPath $check.data_directory) {
        throw '--check-config created the runtime data directory'
    }

    $startConfigArgument = '--config="' + $configPath + '"'
    $process = Start-Process -FilePath $binaryPath `
        -ArgumentList @($startConfigArgument, '--run-for=4s') `
        -RedirectStandardOutput $stdoutPath `
        -RedirectStandardError $stderrPath `
        -WindowStyle Hidden `
        -PassThru

    $health = $null
    $deadline = [DateTime]::UtcNow.AddSeconds(3)
    while ([DateTime]::UtcNow -lt $deadline -and -not $health) {
        if ($process.HasExited) {
            break
        }
        try {
            $health = Invoke-RestMethod -Uri "http://127.0.0.1:$port/health/live" -Method Get -TimeoutSec 1
        }
        catch {
            Start-Sleep -Milliseconds 100
        }
    }
    if (-not $health) {
        throw 'liveness endpoint did not become available'
    }
    if ($health.status -ne 'ok' -or $health.service -ne 'exam-monitor' -or $health.mode -ne 'record-only') {
        throw "unexpected liveness response: $($health | ConvertTo-Json -Compress)"
    }

    if (-not $process.WaitForExit(10000)) {
        throw 'exam-monitor did not exit within the smoke timeout'
    }
    if (-not $process.HasExited) {
        throw 'exam-monitor remained running after the smoke timeout'
    }

    $logRecords = @()
    foreach ($line in @(Get-Content -LiteralPath $stderrPath -Encoding UTF8)) {
        if ($line.Trim()) {
            $logRecords += $line | ConvertFrom-Json
        }
    }
    if ($logRecords.Count -lt 2) {
        throw "expected startup and shutdown logs, got $($logRecords.Count)"
    }
    foreach ($record in $logRecords) {
        foreach ($field in @('time', 'level', 'service', 'build_version', 'component', 'event')) {
            if (-not $record.PSObject.Properties[$field]) {
                throw "structured log missing $field"
            }
        }
    }
    if (-not ($logRecords.event -contains 'started') -or -not ($logRecords.event -contains 'stopped')) {
        throw 'structured logs do not contain both started and stopped events'
    }

    # Start-Process on Windows PowerShell 5.1 does not reliably populate ExitCode.
    # A short foreground run verifies the executable's actual clean exit code.
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $cleanExitOutput = @(& $binaryPath "--config=$configPath" '--run-for=500ms' 2>&1)
        $cleanExitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    if ($cleanExitCode -ne 0) {
        throw "foreground clean-exit check failed with code $cleanExitCode`: $($cleanExitOutput -join [Environment]::NewLine)"
    }

    Write-Output "M0 smoke passed on http://127.0.0.1:$port/health/live"
}
catch {
    if (Test-Path -LiteralPath $stderrPath) {
        Write-Warning ((Get-Content -Raw -LiteralPath $stderrPath -Encoding UTF8).Trim())
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
