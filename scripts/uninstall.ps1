[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$AppRoot = (Join-Path $env:LOCALAPPDATA 'ExamMonitor'),
    [string]$TaskName = 'ExamMonitor Recorder Core',
    [switch]$PlanOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'process-control.ps1')
function Invoke-TaskCommand {
    param([string[]]$Arguments)
    $savedPreference = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & schtasks.exe @Arguments 2>&1 | Out-Null; return $LASTEXITCODE } finally { $ErrorActionPreference = $savedPreference }
}
if (-not [IO.Path]::IsPathRooted($AppRoot)) { throw 'UNINSTALL_ABSOLUTE_PATH_REQUIRED' }
$result = [ordered]@{ schema_version = 1; status = 'planned'; task_name = $TaskName; app_root = $AppRoot; data_preserved = $true }
if ($PlanOnly) { $result | ConvertTo-Json; return }

Stop-ExamMonitorManagedProcesses -AppRoot $AppRoot -TaskName $TaskName -ErrorPrefix 'UNINSTALL'
$deleteExit = Invoke-TaskCommand -Arguments @('/Delete','/TN',$TaskName,'/F')
if ($deleteExit -ne 0) {
    $queryExit = Invoke-TaskCommand -Arguments @('/Query','/TN',$TaskName)
    if ($queryExit -eq 0) { throw 'UNINSTALL_TASK_DELETE_FAILED' }
}
$result.status = 'uninstalled'
$result | ConvertTo-Json
