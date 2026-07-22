[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BinaryPath,
    [Parameter(Mandatory)][string]$ConfigPath,
    [Parameter(Mandatory)][string]$DestinationDirectory,
    [ValidateSet('metadata', 'full')][string]$Type = 'metadata',
    [int]$InjectFailureAfterFiles = 0
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Utf8 {
    param([string]$Path, [object]$Value)
    [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 12), (New-Object Text.UTF8Encoding($false)))
}

function Test-PathOverlap {
    param([string]$First, [string]$Second)
    $a = [IO.Path]::GetFullPath($First).TrimEnd('\') + '\'
    $b = [IO.Path]::GetFullPath($Second).TrimEnd('\') + '\'
    return $a.StartsWith($b, [StringComparison]::OrdinalIgnoreCase) -or $b.StartsWith($a, [StringComparison]::OrdinalIgnoreCase)
}

foreach ($path in @($BinaryPath, $ConfigPath, $DestinationDirectory)) { if (-not [IO.Path]::IsPathRooted($path)) { throw 'BACKUP_ABSOLUTE_PATH_REQUIRED' } }
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf) -or -not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)) { throw 'BACKUP_INPUT_MISSING' }
$check = ((& $BinaryPath "--config=$ConfigPath" --check-config) | Out-String).Trim() | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $check.status -ne 'ok') { throw 'BACKUP_CONFIG_INVALID' }
$dataDirectory = $check.data_directory
if (Test-PathOverlap -First $dataDirectory -Second $DestinationDirectory) { throw 'BACKUP_DESTINATION_OVERLAPS_DATA' }

[void](New-Item -ItemType Directory -Path $DestinationDirectory -Force)
$name = 'exam-monitor-' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$staging = Join-Path $DestinationDirectory ('.' + $name + '.partial')
$final = Join-Path $DestinationDirectory $name
$copied = 0
try {
    [void](New-Item -ItemType Directory -Path (Join-Path $staging 'database') -Force)
    [void](New-Item -ItemType Directory -Path (Join-Path $staging 'configuration') -Force)
    [void](New-Item -ItemType Directory -Path (Join-Path $staging 'metadata') -Force)
    $databasePath = Join-Path $staging 'database\exam-monitor.db'
    & $BinaryPath "--config=$ConfigPath" "--backup-database=$databasePath" | Out-Null
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $databasePath -PathType Leaf)) { throw 'BACKUP_DATABASE_SNAPSHOT_FAILED' }
    Copy-Item -LiteralPath $ConfigPath -Destination (Join-Path $staging 'configuration\exam-monitor.json')
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'restore.ps1') -Destination (Join-Path $staging 'metadata\restore.ps1')
    [IO.File]::WriteAllText((Join-Path $staging 'metadata\build-version.json'), ((& $BinaryPath --version) | Out-String).Trim(), (New-Object Text.UTF8Encoding($false)))
    [IO.File]::WriteAllText((Join-Path $staging 'metadata\schema-version.json'), ((& $BinaryPath "--config=$ConfigPath" --schema-info) | Out-String).Trim(), (New-Object Text.UTF8Encoding($false)))
	$mediaManifestJSON = ((& $BinaryPath "--media-manifest-database=$databasePath") | Out-String).Trim()
	if ($LASTEXITCODE -ne 0) { throw 'BACKUP_MEDIA_MANIFEST_FAILED' }
	$mediaManifest = $mediaManifestJSON | ConvertFrom-Json
	[IO.File]::WriteAllText((Join-Path $staging 'metadata\media-manifest.json'), $mediaManifestJSON, (New-Object Text.UTF8Encoding($false)))

    $files = [Collections.Generic.List[object]]::new()
    foreach ($relative in @('database\exam-monitor.db', 'configuration\exam-monitor.json', 'metadata\restore.ps1', 'metadata\build-version.json', 'metadata\schema-version.json', 'metadata\media-manifest.json')) {
        $path = Join-Path $staging $relative
        $kind = if ($relative.StartsWith('database')) { 'database' } elseif ($relative.StartsWith('configuration')) { 'config' } else { 'metadata' }
        $files.Add([ordered]@{ relative_path = $relative.Replace('\', '/'); backup_path = $relative.Replace('\', '/'); kind = $kind; included = $true; size_bytes = (Get-Item -LiteralPath $path).Length; sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() })
    }

    foreach ($entry in @($mediaManifest.media)) {
            if ($entry.status -notin @('accepted','restored') -or $entry.managed_relative_path -notmatch '^accepted/[0-9a-f]{64}\.media$' -or $entry.sha256 -notmatch '^[0-9a-f]{64}$') { throw 'BACKUP_DATABASE_MEDIA_PATH_INVALID' }
            $sourcePath = Join-Path (Join-Path $dataDirectory 'media') ($entry.managed_relative_path.Replace('/', '\'))
            if (-not (Test-Path -LiteralPath $sourcePath -PathType Leaf)) { throw "BACKUP_ACCEPTED_MEDIA_MISSING:$($entry.managed_relative_path)" }
            $media = Get-Item -LiteralPath $sourcePath
            $hash = (Get-FileHash -LiteralPath $sourcePath -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($media.Length -ne $entry.size_bytes -or $hash -ne $entry.sha256) { throw "BACKUP_MANAGED_MEDIA_HASH_MISMATCH:$($entry.managed_relative_path)" }
            $backupRelative = 'media/' + $entry.managed_relative_path
            $included = $Type -eq 'full'
            if ($included) {
				$target = Join-Path $staging ($backupRelative.Replace('/', '\'))
                [void](New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force)
                Copy-Item -LiteralPath $sourcePath -Destination $target
                $copied++
                if ($InjectFailureAfterFiles -gt 0 -and $copied -ge $InjectFailureAfterFiles) { throw 'BACKUP_INJECTED_INTERRUPTION' }
            }
            $files.Add([ordered]@{ relative_path = $entry.managed_relative_path; backup_path = $backupRelative; kind = 'media'; included = $included; size_bytes = $media.Length; sha256 = $hash })
    }

    $manifest = [ordered]@{
        schema_version = 1
        type = $Type
        status = 'complete'
        created_at_utc = [DateTime]::UtcNow.ToString('o')
        source_data_directory = $dataDirectory
        coverage = if ($Type -eq 'full') { 'database, configuration, schema/build metadata, and listed managed media bodies' } else { 'database, configuration, schema/build metadata, and media checksums only; media bodies are excluded' }
        files = $files
    }
    $manifestPath = Join-Path $staging 'manifest.json'
    Write-Utf8 -Path $manifestPath -Value $manifest
    foreach ($file in $files) {
        if (-not $file.included) { continue }
        $path = Join-Path $staging ($file.backup_path.Replace('/', '\'))
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $file.sha256) { throw "BACKUP_VERIFICATION_FAILED:$($file.backup_path)" }
    }
    Move-Item -LiteralPath $staging -Destination $final

    if ($Type -eq 'full') {
        $manifestFinal = Join-Path $final 'manifest.json'
        $markerDirectory = Join-Path $dataDirectory 'backup'
        [void](New-Item -ItemType Directory -Path $markerDirectory -Force)
        $markerPath = Join-Path $markerDirectory 'latest-full.json'
        $markerTemporary = "$markerPath.$([guid]::NewGuid().ToString('N')).tmp"
        Write-Utf8 -Path $markerTemporary -Value ([ordered]@{ schema_version = 1; manifest_path = $manifestFinal; manifest_sha256 = (Get-FileHash -LiteralPath $manifestFinal -Algorithm SHA256).Hash.ToLowerInvariant(); completed_at_utc = [DateTime]::UtcNow.ToString('o') })
        Move-Item -LiteralPath $markerTemporary -Destination $markerPath -Force
    }
    [ordered]@{ schema_version = 1; status = 'complete'; type = $Type; backup_directory = $final; manifest_path = (Join-Path $final 'manifest.json') } | ConvertTo-Json
}
catch {
    if (Test-Path -LiteralPath $staging -PathType Container) { Remove-Item -LiteralPath $staging -Recurse -Force }
    throw
}
