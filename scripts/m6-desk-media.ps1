[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InboxDirectory,
    [Parameter(Mandatory)][string]$FFmpegPath,
    [Parameter(Mandatory)][string]$ExpectedFFmpegSHA256,
    [Parameter(Mandatory)][string]$DeviceName,
    [Parameter(Mandatory)][string]$StateDirectory,
    [string]$CollectorID = 'desk.media',
    [ValidateRange(1, 600)][int]$SegmentSeconds = 300,
    [ValidateRange(1, 12)][int]$SegmentCount = 3,
    [ValidateRange(0, 86400000)][int64]$ClockErrorMS = 1000,
    [ValidateRange(5, 600)][int]$AcceptanceTimeoutSeconds = 120,
    [switch]$PlanOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$utf8NoBom = New-Object Text.UTF8Encoding($false)

function Assert-AbsoluteRegularDirectory {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code)
    if (-not [IO.Path]::IsPathRooted($Path)) { throw $Code }
    $resolved = [IO.Path]::GetFullPath($Path).TrimEnd('\')
    $item = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw $Code }
    return $resolved
}

function Assert-AbsoluteRegularFile {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)][string]$Code)
    if (-not [IO.Path]::IsPathRooted($Path)) { throw $Code }
    $resolved = [IO.Path]::GetFullPath($Path)
    $item = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw $Code }
    return $resolved
}

function Assert-Identifier {
    param([Parameter(Mandatory)][string]$Value, [Parameter(Mandatory)][string]$Code)
    $invalidCharacters = [char[]]@([char]13, [char]10, [char]9, [char]34)
    if ([string]::IsNullOrWhiteSpace($Value) -or $Value.Length -gt 128 -or $Value.IndexOfAny($invalidCharacters) -ge 0) { throw $Code }
}

function Write-JsonAtomic {
    param([Parameter(Mandatory)][string]$Path, [Parameter(Mandatory)]$Value)
    $temporary = "$Path.$([guid]::NewGuid().ToString('N')).tmp"
    [IO.File]::WriteAllText($temporary, ($Value | ConvertTo-Json -Depth 12), $utf8NoBom)
    Move-Item -LiteralPath $temporary -Destination $Path
}

function Write-PublisherRecord {
    param([Parameter(Mandatory)][string]$Directory, [Parameter(Mandatory)]$Value)
    $name = [DateTime]::Parse([string]$Value.started_at_utc).ToUniversalTime().ToString('yyyyMMddTHHmmssfffffffZ') + '.json'
    Write-JsonAtomic -Path (Join-Path $Directory $name) -Value $Value
    [IO.File]::AppendAllText((Join-Path $Directory 'runs.jsonl'), (($Value | ConvertTo-Json -Depth 12 -Compress) + [Environment]::NewLine), $utf8NoBom)
}

Assert-Identifier -Value $CollectorID -Code 'M6_MEDIA_COLLECTOR_ID_INVALID'
Assert-Identifier -Value $DeviceName -Code 'M6_MEDIA_DEVICE_NAME_INVALID'
$inbox = Assert-AbsoluteRegularDirectory -Path $InboxDirectory -Code 'M6_MEDIA_INBOX_INVALID'
$ffmpeg = Assert-AbsoluteRegularFile -Path $FFmpegPath -Code 'M6_MEDIA_FFMPEG_INVALID'
$state = Assert-AbsoluteRegularDirectory -Path $StateDirectory -Code 'M6_MEDIA_STATE_DIRECTORY_INVALID'
if ($ExpectedFFmpegSHA256 -notmatch '^[0-9a-f]{64}$' -or (Get-FileHash -LiteralPath $ffmpeg -Algorithm SHA256).Hash.ToLowerInvariant() -ne $ExpectedFFmpegSHA256) { throw 'M6_MEDIA_FFMPEG_CHANGED' }
if ($SegmentSeconds * $SegmentCount -gt 3600) { throw 'M6_MEDIA_RUN_DURATION_INVALID' }

$plan = [ordered]@{
    schema_version = 1; status = 'planned'; inbox_directory = $inbox; ffmpeg_path = $ffmpeg; ffmpeg_sha256 = $ExpectedFFmpegSHA256; device_name = $DeviceName
    collector_id = $CollectorID; segment_seconds = $SegmentSeconds; segment_count = $SegmentCount
    clock_error_ms = $ClockErrorMS; acceptance_timeout_seconds = $AcceptanceTimeoutSeconds
}
if ($PlanOnly) { $plan | ConvertTo-Json -Depth 8; return }

$runStarted = [DateTime]::UtcNow
$segments = @()
$run = [ordered]@{
    schema_version = 1; started_at_utc = $runStarted.ToString('o'); completed_at_utc = $null; status = 'running'
    collector_id = $CollectorID; device_name = $DeviceName; segment_seconds = $SegmentSeconds; requested_segments = $SegmentCount
    published_segments = 0; accepted_segments = 0; segments = @(); error_code = $null; detail = $null
}

try {
    for ($index = 1; $index -le $SegmentCount; $index++) {
        $segmentStarted = [DateTimeOffset]::Now
        $sourceKey = 'm6-desk-' + $segmentStarted.UtcDateTime.ToString('yyyyMMddTHHmmssfffffffZ') + '-' + $index.ToString('00')
        $finalName = "$sourceKey.mp4"
        $capturePath = Join-Path $inbox "$sourceKey.capturing.mp4"
        $finalPath = Join-Path $inbox $finalName
        $sidecarPath = "$finalPath.sidecar.json"
        $readyPath = "$finalPath.ready"
        $confirmationPath = "$finalPath.accepted.json"
        foreach ($path in @($capturePath, $finalPath, $sidecarPath, $readyPath, $confirmationPath)) {
            if (Test-Path -LiteralPath $path) { throw 'M6_MEDIA_PUBLISH_TARGET_EXISTS' }
        }

        $arguments = @(
            '-hide_banner', '-loglevel', 'warning', '-nostdin', '-y',
            '-f', 'dshow', '-rtbufsize', '256M', '-i', "video=$DeviceName",
            '-t', ([string]$SegmentSeconds), '-an', '-vf', 'fps=10,scale=640:-2',
            '-c:v', 'libx264', '-preset', 'ultrafast', '-crf', '28', '-pix_fmt', 'yuv420p', '-movflags', '+faststart',
            $capturePath
        )
        $savedPreference = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $captureOutput = @(& $ffmpeg @arguments 2>&1)
            $captureExit = $LASTEXITCODE
        }
        finally { $ErrorActionPreference = $savedPreference }
        if ($captureExit -ne 0 -or -not (Test-Path -LiteralPath $capturePath -PathType Leaf)) {
            $tail = (($captureOutput | Select-Object -Last 20 | ForEach-Object { [string]$_ }) -join [Environment]::NewLine)
            if ($tail.Length -gt 4096) { $tail = $tail.Substring($tail.Length - 4096) }
            throw "M6_MEDIA_CAPTURE_FAILED:$captureExit`:$tail"
        }
        $segmentEnded = [DateTimeOffset]::Now
        $captureItem = Get-Item -LiteralPath $capturePath -Force
        if ($captureItem.Length -le 0 -or $captureItem.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'M6_MEDIA_CAPTURE_INVALID' }
        $sha256 = (Get-FileHash -LiteralPath $capturePath -Algorithm SHA256).Hash.ToLowerInvariant()
        Move-Item -LiteralPath $capturePath -Destination $finalPath
        $sidecar = [ordered]@{
            schema_version = 1; complete = $true; collector_id = $CollectorID; source_idempotency_key = $sourceKey
            device_start_raw = $segmentStarted.ToString('yyyy-MM-ddTHH:mm:ss.fffffffzzz')
            device_end_raw = $segmentEnded.ToString('yyyy-MM-ddTHH:mm:ss.fffffffzzz')
            clock_offset_ms = 0; clock_error_ms = $ClockErrorMS; size_bytes = $captureItem.Length; sha256 = $sha256; media_type = 'video'
        }
        Write-JsonAtomic -Path $sidecarPath -Value $sidecar
        $readyTemporary = "$readyPath.$([guid]::NewGuid().ToString('N')).tmp"
        [IO.File]::WriteAllBytes($readyTemporary, [byte[]]@())
        Move-Item -LiteralPath $readyTemporary -Destination $readyPath
        $segments += [ordered]@{
            source_idempotency_key = $sourceKey; media_path = $finalPath; confirmation_path = $confirmationPath
            sha256 = $sha256; size_bytes = $captureItem.Length; device_start_raw = $sidecar.device_start_raw; device_end_raw = $sidecar.device_end_raw
            accepted = $false; media_segment_id = $null; accepted_at_utc = $null
        }
        $run.published_segments = $segments.Count
        $run.segments = $segments
    }

    foreach ($segment in $segments) {
        $deadline = [DateTime]::UtcNow.AddSeconds($AcceptanceTimeoutSeconds)
        $confirmation = $null
        do {
            if (Test-Path -LiteralPath $segment.confirmation_path -PathType Leaf) {
                try { $confirmation = Get-Content -Raw -Encoding UTF8 -LiteralPath $segment.confirmation_path | ConvertFrom-Json } catch { $confirmation = $null }
                if ($null -ne $confirmation -and $confirmation.schema_version -eq 1 -and $confirmation.collector_id -eq $CollectorID -and $confirmation.source_idempotency_key -eq $segment.source_idempotency_key -and $confirmation.sha256 -eq $segment.sha256 -and [int64]$confirmation.media_segment_id -gt 0) { break }
                $confirmation = $null
            }
            Start-Sleep -Milliseconds 500
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($null -eq $confirmation) { throw 'M6_MEDIA_ACCEPTANCE_TIMEOUT' }
        $segment.accepted = $true
        $segment.media_segment_id = [int64]$confirmation.media_segment_id
        $segment.accepted_at_utc = [DateTime]::UtcNow.ToString('o')
        $run.accepted_segments = @($segments | Where-Object { $_.accepted }).Count
        $run.segments = $segments
    }
    $run.status = 'passed'
}
catch {
    $run.status = 'failed'
    $run.error_code = if ($_.Exception.Message -match '^([A-Z0-9_]+)') { $Matches[1] } else { 'M6_MEDIA_PUBLISH_FAILED' }
    $run.detail = $_.Exception.Message
    throw
}
finally {
    $run.completed_at_utc = [DateTime]::UtcNow.ToString('o')
    $run.published_segments = $segments.Count
    $run.accepted_segments = @($segments | Where-Object { $_.accepted }).Count
    $run.segments = $segments
    Write-PublisherRecord -Directory $state -Value $run
}

$run | ConvertTo-Json -Depth 12
