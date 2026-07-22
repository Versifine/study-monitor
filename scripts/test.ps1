[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

function Restore-EnvironmentVariable {
    param([string]$Name, [AllowNull()][string]$Value)

    if ($null -eq $Value) {
        Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    }
    else {
        Set-Item "Env:$Name" $Value
    }
}

function Invoke-Go {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
}

$oldGOTOOLCHAIN = $env:GOTOOLCHAIN
$oldGOPROXY = $env:GOPROXY
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
Push-Location $repoRoot
try {
    $requiredGo = ((Select-String -Path 'go.mod' -Pattern '^go\s+(.+)$').Matches[0].Groups[1].Value).Trim()
    $actualGo = ((& go env GOVERSION) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "go env GOVERSION failed with exit code $LASTEXITCODE"
    }
    if ($actualGo -ne "go$requiredGo") {
        throw "Go version mismatch: required go$requiredGo, found $actualGo"
    }

    & (Join-Path $PSScriptRoot 'build-web.ps1') -Check
    & node --test 'web/tests/*.test.mjs'
    if ($LASTEXITCODE -ne 0) {
        throw "dashboard state tests failed with exit code $LASTEXITCODE"
    }

    $goFiles = @(Get-ChildItem -Path 'cmd', 'internal' -Recurse -File -Filter '*.go' | ForEach-Object { $_.FullName })
    $unformatted = @(& gofmt -l $goFiles)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed with exit code $LASTEXITCODE"
    }
    if ($unformatted.Count -ne 0) {
        throw "Go files require formatting:`n$($unformatted -join [Environment]::NewLine)"
    }

    Invoke-Go list -mod=vendor ./...
    Invoke-Go vet -mod=vendor ./...
    Invoke-Go test -mod=vendor -count=1 ./...
}
finally {
    Restore-EnvironmentVariable -Name 'GOTOOLCHAIN' -Value $oldGOTOOLCHAIN
    Restore-EnvironmentVariable -Name 'GOPROXY' -Value $oldGOPROXY
    Pop-Location
}
