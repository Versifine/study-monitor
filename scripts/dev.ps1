[CmdletBinding()]
param(
    [string]$Config,
    [switch]$CheckConfig,
    [switch]$Version,
    [TimeSpan]$RunFor = [TimeSpan]::Zero
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

function Restore-EnvironmentVariable {
    param([string]$Name, [bool]$WasPresent, [AllowNull()][string]$Value)

    if ($WasPresent) {
        Set-Item "Env:$Name" $Value
    }
    else {
        Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    }
}

if ($Config -and -not [IO.Path]::IsPathRooted($Config)) {
    $Config = [IO.Path]::GetFullPath((Join-Path (Get-Location).Path $Config))
}

$hadGOTOOLCHAIN = Test-Path 'Env:GOTOOLCHAIN'
$oldGOTOOLCHAIN = $env:GOTOOLCHAIN
$hadGOPROXY = Test-Path 'Env:GOPROXY'
$oldGOPROXY = $env:GOPROXY
$hadDataDirectory = Test-Path 'Env:EXAM_MONITOR_DATA_DIRECTORY'
$oldDataDirectory = $env:EXAM_MONITOR_DATA_DIRECTORY
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
if (-not $hadDataDirectory) {
    $env:EXAM_MONITOR_DATA_DIRECTORY = Join-Path $repoRoot 'data'
}

Push-Location $repoRoot
try {
    & (Join-Path $PSScriptRoot 'build-web.ps1') -Check
    $arguments = @('-mod=vendor', './cmd/exam-monitor')
    if ($Config) {
        $arguments += "--config=$Config"
    }
    if ($CheckConfig) {
        $arguments += '--check-config'
    }
    if ($Version) {
        $arguments += '--version'
    }
    if ($RunFor -gt [TimeSpan]::Zero) {
        $milliseconds = [Math]::Ceiling($RunFor.TotalMilliseconds)
        $arguments += "--run-for=$($milliseconds)ms"
    }

    & go run @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go run failed with exit code $LASTEXITCODE"
    }
}
finally {
    Restore-EnvironmentVariable -Name 'GOTOOLCHAIN' -WasPresent $hadGOTOOLCHAIN -Value $oldGOTOOLCHAIN
    Restore-EnvironmentVariable -Name 'GOPROXY' -WasPresent $hadGOPROXY -Value $oldGOPROXY
    Restore-EnvironmentVariable -Name 'EXAM_MONITOR_DATA_DIRECTORY' -WasPresent $hadDataDirectory -Value $oldDataDirectory
    Pop-Location
}
