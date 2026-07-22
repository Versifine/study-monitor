[CmdletBinding()]
param(
    [switch]$Check
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

Push-Location $repoRoot
try {
    $package = Get-Content -Raw -Encoding UTF8 'web/package.json' | ConvertFrom-Json
    $requiredNode = [string]$package.engines.node
    $actualNode = ((& node --version) | Out-String).Trim().TrimStart('v')
    if ($LASTEXITCODE -ne 0 -or $actualNode -ne $requiredNode) {
        throw "Node.js version mismatch: required v$requiredNode, found v$actualNode"
    }

    $arguments = @('--no-warnings', 'web/build.mjs')
    if ($Check) {
        $arguments += '--check'
    }
    & node @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "dashboard build failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
