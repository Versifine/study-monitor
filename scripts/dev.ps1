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

if ($Config -and -not [IO.Path]::IsPathRooted($Config)) {
    $Config = [IO.Path]::GetFullPath((Join-Path (Get-Location).Path $Config))
}

Push-Location $repoRoot
try {
    $arguments = @('./cmd/exam-monitor')
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
    Pop-Location
}
