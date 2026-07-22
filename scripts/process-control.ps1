Set-StrictMode -Version Latest

function Invoke-ExamMonitorTaskCommand {
    param([string[]]$Arguments)
    $savedPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { & schtasks.exe @Arguments 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $savedPreference }
}

function Stop-ExamMonitorManagedProcesses {
    param(
        [Parameter(Mandatory)][string]$AppRoot,
        [Parameter(Mandatory)][string]$TaskName,
        [string]$ErrorPrefix = 'PROCESS_CONTROL'
    )
    if (-not [IO.Path]::IsPathRooted($AppRoot) -or [string]::IsNullOrWhiteSpace($TaskName) -or $TaskName.IndexOfAny([char[]]"`r`n") -ge 0) { throw "${ErrorPrefix}_ARGUMENT_INVALID" }
    $null = Invoke-ExamMonitorTaskCommand -Arguments @('/End','/TN',$TaskName)
    $statePath = Join-Path $AppRoot 'state\supervisor-state.json'
    if (Test-Path -LiteralPath $statePath -PathType Leaf) {
        try { $state = Get-Content -LiteralPath $statePath -Raw -Encoding UTF8 | ConvertFrom-Json } catch { $state = $null }
        $stateProperties = if ($null -eq $state) { @() } else { @($state.PSObject.Properties.Name) }
        $hasSupervisorIdentity = @('supervisor_pid','supervisor_started_at_utc','supervisor_executable') | Where-Object { $_ -notin $stateProperties } | Measure-Object | Select-Object -ExpandProperty Count
        if ($null -ne $state -and $hasSupervisorIdentity -eq 0 -and [int64]$state.supervisor_pid -gt 0 -and -not [string]::IsNullOrWhiteSpace([string]$state.supervisor_started_at_utc) -and -not [string]::IsNullOrWhiteSpace([string]$state.supervisor_executable)) {
            $supervisor = Get-Process -Id ([int]$state.supervisor_pid) -ErrorAction SilentlyContinue
            if ($null -ne $supervisor) {
                $identityMatches = $false
                try {
                    $actualExecutable = [IO.Path]::GetFullPath([string]$supervisor.Path)
                    $expectedExecutable = [IO.Path]::GetFullPath([string]$state.supervisor_executable)
                    $actualStarted = $supervisor.StartTime.ToUniversalTime()
                    $expectedStarted = [DateTime]::Parse([string]$state.supervisor_started_at_utc).ToUniversalTime()
                    $validName = [IO.Path]::GetFileName($actualExecutable) -in @('powershell.exe','pwsh.exe')
                    $identityMatches = $validName -and $actualExecutable.Equals($expectedExecutable, [StringComparison]::OrdinalIgnoreCase) -and [Math]::Abs(($actualStarted - $expectedStarted).TotalSeconds) -le 1
                }
                catch { $identityMatches = $false }
                if ($identityMatches) {
                    Stop-Process -Id $supervisor.Id -Force -ErrorAction SilentlyContinue
                    try { Wait-Process -Id $supervisor.Id -Timeout 5 -ErrorAction Stop } catch { if (Get-Process -Id $supervisor.Id -ErrorAction SilentlyContinue) { throw "${ErrorPrefix}_SUPERVISOR_STOP_FAILED" } }
                }
            }
        }
    }
    $releaseRoot = [IO.Path]::GetFullPath((Join-Path $AppRoot 'releases')).TrimEnd('\') + '\'
    $deadline = [DateTime]::UtcNow.AddSeconds(8)
    do {
        $managed = @()
        foreach ($process in @(Get-Process -Name 'exam-monitor' -ErrorAction SilentlyContinue)) {
            try { $path = [IO.Path]::GetFullPath([string]$process.Path) } catch { continue }
            if ([IO.Path]::GetFileName($path) -eq 'exam-monitor.exe' -and $path.StartsWith($releaseRoot, [StringComparison]::OrdinalIgnoreCase)) {
                $managed += $process
            }
        }
        if ($managed.Count -eq 0) { return }
        foreach ($process in $managed) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue }
        Start-Sleep -Milliseconds 100
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "${ErrorPrefix}_MANAGED_PROCESS_STOP_FAILED"
}
