[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot

function Invoke-Go {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
}

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

    $goFiles = @(Get-ChildItem -Path 'cmd', 'internal' -Recurse -File -Filter '*.go' | ForEach-Object { $_.FullName })
    $unformatted = @(& gofmt -l $goFiles)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed with exit code $LASTEXITCODE"
    }
    if ($unformatted.Count -ne 0) {
        throw "Go files require formatting:`n$($unformatted -join [Environment]::NewLine)"
    }

    Invoke-Go list -mod=readonly ./...
    Invoke-Go vet ./...
    Invoke-Go test -count=1 ./...
}
finally {
    Pop-Location
}
