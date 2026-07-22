[CmdletBinding()]
param([switch]$SkipOperationalSmoke)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$oldGOTOOLCHAIN = $env:GOTOOLCHAIN
$oldGOPROXY = $env:GOPROXY
$env:GOTOOLCHAIN = 'local'
$env:GOPROXY = 'off'
Push-Location $repoRoot
try {
    & go test -mod=vendor -count=1 ./internal/eventstore -run 'TestDatabaseBusyIsBoundedAndRetryable|TestReadinessDistinguishesWritableBusyAndReadOnly|TestConsistentDatabaseBackupIsVerifiedAndDoesNotReplaceExistingTarget'
    if ($LASTEXITCODE -ne 0) { throw 'FAULT_INJECTION_DATABASE_FAILED' }
    & go test -mod=vendor -count=1 ./internal/collectors -run 'TestActivityWatchFailureIsIsolatedPerCollector|TestActivityWatchDoesNotFollowRedirects'
    if ($LASTEXITCODE -ne 0) { throw 'FAULT_INJECTION_NETWORK_FAILED' }
    & go test -mod=vendor -count=1 ./internal/operations -run 'TestInjectedDiskLevelsProtectMediaBeforeCoreWrites|TestRetentionDefaultsOffAndRequiresVerifiedFullBackup|TestRetentionRecoversDeleteBeforeDatabaseCommit|TestTemporaryCleanupOnlyRemovesOldUnreferencedCorePartial'
    if ($LASTEXITCODE -ne 0) { throw 'FAULT_INJECTION_STORAGE_FAILED' }
    if (-not $SkipOperationalSmoke) { & (Join-Path $PSScriptRoot 'smoke.ps1') }
    Write-Output 'M4 fault injection passed: busy/read-only database, network isolation, injected disk levels, safe retention/temp recovery, plus process/backup/restore/corruption/rollback scenarios in operational smoke'
}
finally {
    if ($null -eq $oldGOTOOLCHAIN) { Remove-Item Env:GOTOOLCHAIN -ErrorAction SilentlyContinue } else { $env:GOTOOLCHAIN = $oldGOTOOLCHAIN }
    if ($null -eq $oldGOPROXY) { Remove-Item Env:GOPROXY -ErrorAction SilentlyContinue } else { $env:GOPROXY = $oldGOPROXY }
    Pop-Location
}
