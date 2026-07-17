[CmdletBinding()]
param(
    [string]$OutputDirectory = 'bin',
    [string]$Version
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$versionPackage = 'github.com/Versifine/study-monitor/internal/version'

function Restore-EnvironmentVariable {
    param([string]$Name, [AllowNull()][string]$Value)

    if ($null -eq $Value) {
        Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    }
    else {
        Set-Item "Env:$Name" $Value
    }
}

$oldGOTOOLCHAIN = $env:GOTOOLCHAIN
$oldGOPROXY = $env:GOPROXY
$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
Push-Location $repoRoot
try {
    $requiredGo = ((Select-String -Path 'go.mod' -Pattern '^go\s+(.+)$').Matches[0].Groups[1].Value).Trim()
    $actualGo = ((& go env GOVERSION) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $actualGo -ne "go$requiredGo") {
        throw "Go version mismatch: required go$requiredGo, found $actualGo"
    }

    if (-not $Version) {
        $Version = (Get-Content -Raw -Encoding UTF8 'VERSION').Trim()
    }
    if ($Version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') {
        throw "Invalid VERSION value: $Version"
    }

    $commit = ((& git rev-parse --verify HEAD) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $commit) {
        throw 'Unable to determine Git commit'
    }
    $repositoryStatus = @(& git status --porcelain=v1 --untracked-files=all)
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to determine Git worktree state'
    }
    if ($repositoryStatus.Count -ne 0) {
        $commit = "$commit-dirty"
    }

    if (-not [IO.Path]::IsPathRooted($OutputDirectory)) {
        $OutputDirectory = Join-Path $repoRoot $OutputDirectory
    }
    [void](New-Item -ItemType Directory -Path $OutputDirectory -Force)
    $binaryPath = Join-Path $OutputDirectory 'exam-monitor.exe'
    $buildTime = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
    $ldflags = "-X $versionPackage.Version=$Version -X $versionPackage.Commit=$commit -X $versionPackage.BuildTime=$buildTime"

    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    & go build -mod=vendor -trimpath -buildvcs=false -ldflags $ldflags -o $binaryPath ./cmd/exam-monitor
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }

    $versionJSON = ((& $binaryPath --version) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "built binary --version failed with exit code $LASTEXITCODE"
    }
    $versionInfo = $versionJSON | ConvertFrom-Json
    if ($versionInfo.version -ne $Version -or $versionInfo.commit -ne $commit -or $versionInfo.build_time_utc -ne $buildTime) {
        throw "built binary version metadata mismatch: $versionJSON"
    }

    Write-Output $binaryPath
}
finally {
    Restore-EnvironmentVariable -Name 'GOTOOLCHAIN' -Value $oldGOTOOLCHAIN
    Restore-EnvironmentVariable -Name 'GOPROXY' -Value $oldGOPROXY
    Restore-EnvironmentVariable -Name 'GOOS' -Value $oldGOOS
    Restore-EnvironmentVariable -Name 'GOARCH' -Value $oldGOARCH
    Restore-EnvironmentVariable -Name 'CGO_ENABLED' -Value $oldCGO
    Pop-Location
}
