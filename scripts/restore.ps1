[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BackupDirectory,
    [Parameter(Mandatory)][string]$VerifierBinaryPath,
    [string]$TargetDirectory,
    [switch]$ConfirmOverwrite,
    [int]$InjectFailureAfterFiles = 0
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-Utf8 {
    param([string]$Path, [object]$Value)
    [IO.File]::WriteAllText($Path, ($Value | ConvertTo-Json -Depth 12), (New-Object Text.UTF8Encoding($false)))
}

function Resolve-ContainedPath {
    param([string]$Root, [string]$Relative)
    if ([string]::IsNullOrWhiteSpace($Relative) -or [IO.Path]::IsPathRooted($Relative)) { throw 'RESTORE_MANIFEST_PATH_INVALID' }
    $normalized = $Relative.Replace('/', [IO.Path]::DirectorySeparatorChar).Replace('\', [IO.Path]::DirectorySeparatorChar)
    $rootFull = [IO.Path]::GetFullPath($Root).TrimEnd([char[]]@([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar))
    $candidate = [IO.Path]::GetFullPath((Join-Path $rootFull $normalized))
    $prefix = $rootFull + [IO.Path]::DirectorySeparatorChar
    if (-not $candidate.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'RESTORE_MANIFEST_PATH_INVALID' }
    return $candidate
}

function Assert-RegularFile {
    param([string]$Path, [string]$Code)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw $Code }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw $Code }
    return $item
}

if (-not [IO.Path]::IsPathRooted($BackupDirectory) -or -not [IO.Path]::IsPathRooted($VerifierBinaryPath)) { throw 'RESTORE_ABSOLUTE_PATH_REQUIRED' }
if (-not (Test-Path -LiteralPath $BackupDirectory -PathType Container) -or -not (Test-Path -LiteralPath $VerifierBinaryPath -PathType Leaf)) { throw 'RESTORE_INPUT_MISSING' }
if (-not $TargetDirectory) { $TargetDirectory = Join-Path (Split-Path -Parent $BackupDirectory) ('restored-' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')) }
if (-not [IO.Path]::IsPathRooted($TargetDirectory)) { throw 'RESTORE_TARGET_MUST_BE_ABSOLUTE' }
$manifestPath = Join-Path $BackupDirectory 'manifest.json'
[void](Assert-RegularFile -Path $manifestPath -Code 'RESTORE_MANIFEST_MISSING')
$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
if ($manifest.schema_version -ne 1 -or $manifest.status -ne 'complete' -or $manifest.type -notin @('metadata', 'full')) { throw 'RESTORE_MANIFEST_INVALID' }

$sourcePaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$databaseEntries = 0
$configEntries = 0
foreach ($file in @($manifest.files)) {
    if ($file.kind -notin @('database', 'config', 'metadata', 'media') -or [string]$file.sha256 -notmatch '^[0-9a-f]{64}$' -or [int64]$file.size_bytes -lt 0) { throw 'RESTORE_MANIFEST_INVALID' }
    $backupPath = ([string]$file.backup_path).Replace('\', '/')
    $relativePath = ([string]$file.relative_path).Replace('\', '/')
    switch ($file.kind) {
        'database' { if ($backupPath -ne 'database/exam-monitor.db' -or $relativePath -ne 'database/exam-monitor.db' -or -not $file.included) { throw 'RESTORE_MANIFEST_INVALID' }; $databaseEntries++ }
        'config' { if ($backupPath -ne 'configuration/exam-monitor.json' -or $relativePath -ne 'configuration/exam-monitor.json' -or -not $file.included) { throw 'RESTORE_MANIFEST_INVALID' }; $configEntries++ }
        'metadata' { if ($backupPath -notmatch '^metadata/[A-Za-z0-9._-]+$' -or $relativePath -ne $backupPath -or -not $file.included) { throw 'RESTORE_MANIFEST_INVALID' } }
        'media' {
            if ($relativePath -notmatch '^accepted/[0-9a-f]{64}\.media$' -or $backupPath -ne ('media/' + $relativePath)) { throw 'RESTORE_MANIFEST_PATH_INVALID' }
            if (($manifest.type -eq 'full') -ne [bool]$file.included) { throw 'RESTORE_MANIFEST_INVALID' }
        }
    }
    if (-not $sourcePaths.Add($backupPath)) { throw 'RESTORE_MANIFEST_DUPLICATE_PATH' }
    if (-not $file.included) { continue }
    $source = Resolve-ContainedPath -Root $BackupDirectory -Relative $backupPath
    $sourceItem = Assert-RegularFile -Path $source -Code "RESTORE_BACKUP_CORRUPT:$backupPath"
    if ($sourceItem.Length -ne [int64]$file.size_bytes -or (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant() -ne $file.sha256) { throw "RESTORE_BACKUP_CORRUPT:$backupPath" }
}
if ($databaseEntries -ne 1 -or $configEntries -ne 1) { throw 'RESTORE_MANIFEST_INVALID' }

$staging = "$TargetDirectory.partial-$([guid]::NewGuid().ToString('N'))"
$displaced = $null
if (Test-Path -LiteralPath $TargetDirectory) {
    if (-not $ConfirmOverwrite) { throw 'RESTORE_TARGET_EXISTS_CONFIRM_REQUIRED' }
    $displaced = "$TargetDirectory.pre-restore-$([DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))"
}

try {
    [void](New-Item -ItemType Directory -Path $staging -Force)
    $copied = 0
    $targetPaths = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($file in @($manifest.files)) {
        if (-not $file.included) { continue }
        $backupPath = ([string]$file.backup_path).Replace('\', '/')
        $relativePath = ([string]$file.relative_path).Replace('\', '/')
        $source = Resolve-ContainedPath -Root $BackupDirectory -Relative $backupPath
        switch ($file.kind) {
            'database' { $targetRelative = 'exam-monitor.db' }
            'config' { $targetRelative = 'source-config.json' }
            'media' { $targetRelative = 'media/' + $relativePath }
            'metadata' { $targetRelative = 'metadata/' + [IO.Path]::GetFileName($backupPath) }
        }
        $target = Resolve-ContainedPath -Root $staging -Relative $targetRelative
        if (-not $targetPaths.Add($target)) { throw 'RESTORE_MANIFEST_DUPLICATE_TARGET' }
        [void](New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force)
        Copy-Item -LiteralPath $source -Destination $target
        $targetItem = Assert-RegularFile -Path $target -Code "RESTORE_COPIED_FILE_INVALID:$backupPath"
        if ($targetItem.Length -ne [int64]$file.size_bytes -or (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant() -ne $file.sha256) { throw "RESTORE_COPIED_FILE_INVALID:$backupPath" }
        $copied++
        if ($InjectFailureAfterFiles -gt 0 -and $copied -ge $InjectFailureAfterFiles) { throw 'RESTORE_INJECTED_INTERRUPTION' }
    }
    $database = Join-Path $staging 'exam-monitor.db'
    & $VerifierBinaryPath "--verify-database=$database" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'RESTORE_DATABASE_INTEGRITY_FAILED' }

    $databaseMediaRaw = ((& $VerifierBinaryPath "--media-manifest-database=$database") | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'RESTORE_DATABASE_MEDIA_MANIFEST_FAILED' }
    try { $databaseMedia = $databaseMediaRaw | ConvertFrom-Json } catch { throw 'RESTORE_DATABASE_MEDIA_MANIFEST_FAILED' }
    $metadataMediaPath = Join-Path $staging 'metadata\media-manifest.json'
    [void](Assert-RegularFile -Path $metadataMediaPath -Code 'RESTORE_MEDIA_MANIFEST_MISSING')
    try { $metadataMedia = Get-Content -LiteralPath $metadataMediaPath -Raw -Encoding UTF8 | ConvertFrom-Json } catch { throw 'RESTORE_MEDIA_MANIFEST_INVALID' }
    if ($databaseMedia.schema_version -ne 1 -or $metadataMedia.schema_version -ne 1) { throw 'RESTORE_MEDIA_MANIFEST_INVALID' }
    $manifestMedia = @{}
    foreach ($file in @($manifest.files | Where-Object { $_.kind -eq 'media' })) {
        $key = ([string]$file.relative_path).Replace('\', '/')
        if ($manifestMedia.ContainsKey($key)) { throw 'RESTORE_MEDIA_MANIFEST_DUPLICATE' }
        $manifestMedia[$key] = $file
    }
    $metadataMediaByPath = @{}
    foreach ($entry in @($metadataMedia.media)) {
        $key = ([string]$entry.managed_relative_path).Replace('\', '/')
        if ($metadataMediaByPath.ContainsKey($key)) { throw 'RESTORE_MEDIA_MANIFEST_DUPLICATE' }
        $metadataMediaByPath[$key] = $entry
    }
    $databaseCount = 0
    foreach ($entry in @($databaseMedia.media)) {
        $databaseCount++
        $key = [string]$entry.managed_relative_path
        if (-not $manifestMedia.ContainsKey($key) -or -not $metadataMediaByPath.ContainsKey($key)) { throw "RESTORE_MEDIA_COVERAGE_MISSING:$key" }
        $listed = $manifestMedia[$key]
        $metadataListed = $metadataMediaByPath[$key]
        if ([string]$entry.status -notin @('accepted','restored') -or [string]$entry.sha256 -ne [string]$listed.sha256 -or [int64]$entry.size_bytes -ne [int64]$listed.size_bytes -or [int64]$entry.media_segment_id -ne [int64]$metadataListed.media_segment_id -or [string]$entry.sha256 -ne [string]$metadataListed.sha256 -or [int64]$entry.size_bytes -ne [int64]$metadataListed.size_bytes -or [string]$entry.status -ne [string]$metadataListed.status) { throw "RESTORE_MEDIA_COVERAGE_MISMATCH:$key" }
        if ($manifest.type -eq 'full') {
            if (-not [bool]$listed.included) { throw "RESTORE_MEDIA_BODY_EXCLUDED:$key" }
            $restoredMedia = Resolve-ContainedPath -Root $staging -Relative ('media/' + $key)
            $restoredItem = Assert-RegularFile -Path $restoredMedia -Code "RESTORE_MEDIA_BODY_MISSING:$key"
            if ($restoredItem.Length -ne [int64]$entry.size_bytes -or (Get-FileHash -LiteralPath $restoredMedia -Algorithm SHA256).Hash.ToLowerInvariant() -ne [string]$entry.sha256) { throw "RESTORE_MEDIA_BODY_INVALID:$key" }
        }
    }
    if ($databaseCount -ne $manifestMedia.Count -or $databaseCount -ne $metadataMediaByPath.Count) { throw 'RESTORE_MEDIA_COVERAGE_EXTRA' }

    $sourceConfig = Get-Content -LiteralPath (Join-Path $staging 'source-config.json') -Raw -Encoding UTF8 | ConvertFrom-Json
    $sourceConfig.paths.data_directory = $TargetDirectory
    $restoredConfig = Join-Path $staging 'exam-monitor.json'
    Write-Utf8 -Path $restoredConfig -Value $sourceConfig
    & $VerifierBinaryPath "--config=$restoredConfig" --check-config | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'RESTORE_CONFIG_INVALID' }

    if ($displaced) { Move-Item -LiteralPath $TargetDirectory -Destination $displaced }
    Move-Item -LiteralPath $staging -Destination $TargetDirectory
    [ordered]@{ schema_version = 1; status = 'verified'; backup_type = $manifest.type; restored_directory = $TargetDirectory; previous_directory = $displaced; switched = $false } | ConvertTo-Json
}
catch {
    if (Test-Path -LiteralPath $staging -PathType Container) { Remove-Item -LiteralPath $staging -Recurse -Force }
    if ($displaced -and -not (Test-Path -LiteralPath $TargetDirectory) -and (Test-Path -LiteralPath $displaced)) { Move-Item -LiteralPath $displaced -Destination $TargetDirectory }
    throw
}
