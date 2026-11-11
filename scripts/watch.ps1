# 看板常驻监听：后台跑，断线自动重连。
#
#   scripts\watch.ps1 start    # 后台起一个监听
#   scripts\watch.ps1 stop     # 停掉
#   scripts\watch.ps1 status   # 看在不在
#   scripts\watch.ps1 logs     # 跟着日志看
#   scripts\watch.ps1 run      # 前台跑（调试用）
#
# 认这些环境变量：
#   BOARD_BIN       agent-board.exe 位置（默认找仓库里那份，再找 PATH）
#   BOARD_URL       看板地址（默认 http://127.0.0.1:8765）
#   BOARD_DATA      数据目录（默认 ~\.agent-board，token 也在这）
#   BOARD_TOKEN     Agent token（默认读 <data>\token）
#   BOARD_CONSUMER  消费者名字（默认 main）
#   BOARD_WAIT      长轮询秒数（默认 15，短一点好断线重连）

$ErrorActionPreference = 'Stop'

$Repo = Split-Path -Parent $PSScriptRoot
if (-not $env:BOARD_DATA) { $env:BOARD_DATA = Join-Path $HOME '.agent-board' }
$RunDir = if ($env:BOARD_RUN_DIR) { $env:BOARD_RUN_DIR } else { Join-Path $env:BOARD_DATA 'run' }
$LogFile = if ($env:BOARD_LOG) { $env:BOARD_LOG } else { Join-Path $RunDir 'watch.log' }
$PidFile = Join-Path $RunDir 'watch.pid'
$Consumer = if ($env:BOARD_CONSUMER) { $env:BOARD_CONSUMER } else { 'main' }
$Wait = if ($env:BOARD_WAIT) { $env:BOARD_WAIT } else { '15' }

function Find-Bin {
  if ($env:BOARD_BIN) { return $env:BOARD_BIN }
  foreach ($c in @((Join-Path $Repo 'agent-board.exe'), (Join-Path $env:BOARD_DATA 'agent-board.exe'))) {
    if (Test-Path $c) { return $c }
  }
  $cmd = Get-Command agent-board -ErrorAction SilentlyContinue
  if ($cmd) { return $cmd.Source }
  return $null
}

function Get-WatchPid {
  if (-not (Test-Path $PidFile)) { return $null }
  $raw = (Get-Content $PidFile -Raw).Trim()
  if (-not $raw) { return $null }
  if (Get-Process -Id ([int]$raw) -ErrorAction SilentlyContinue) { return [int]$raw }
  return $null
}

function Watch-Loop($bin) {
  # watch 自己会重连；这层兜住进程整个挂掉的情况。
  while ($true) {
    & $bin watch --consumer $Consumer --wait $Wait
    Write-Output '[watch.ps1] 监听退出，3 秒后重连'
    Start-Sleep -Seconds 3
  }
}

$action = if ($args.Count -ge 1) { $args[0] } else { 'status' }
$bin = Find-Bin
if (-not $bin) { Write-Error "找不到 agent-board：先 go build -o agent-board.exe . ，或设 BOARD_BIN" }

switch ($action) {
  'start' {
    $existing = Get-WatchPid
    if ($existing) { Write-Output "已在运行（pid $existing）"; exit 0 }
    New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
    Remove-Item $LogFile, "$LogFile.err" -ErrorAction SilentlyContinue
    $wrapper = Join-Path $RunDir 'watch-loop.ps1'
    $wrapperBody = @"
`$ErrorActionPreference = 'Continue'
while (`$true) {
  & '$bin' watch --consumer '$Consumer' --wait '$Wait'
  Write-Output '[watch.ps1] 监听退出，3 秒后重连'
  Start-Sleep -Seconds 3
}
"@
    Set-Content -Path $wrapper -Value $wrapperBody -Encoding utf8
    $p = Start-Process -FilePath 'powershell.exe' `
      -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $wrapper) `
      -RedirectStandardOutput $LogFile -RedirectStandardError "$LogFile.err" -WindowStyle Hidden -PassThru
    Set-Content -Path $PidFile -Value $p.Id -Encoding ascii
    Start-Sleep -Milliseconds 800
    if (Get-Process -Id $p.Id -ErrorAction SilentlyContinue) {
      Write-Output "已启动（pid $($p.Id)），日志 $LogFile"
    } else {
      Remove-Item $PidFile -ErrorAction SilentlyContinue
      Write-Error "启动失败，看日志 $LogFile"
    }
  }
  'stop' {
    $pid_ = Get-WatchPid
    if (-not $pid_) { Write-Output '没有在跑'; Remove-Item $PidFile -ErrorAction SilentlyContinue; exit 0 }
    Stop-Process -Id $pid_ -Force -ErrorAction SilentlyContinue
    # 连子进程一起收掉，别留孤儿在轮询。
    & taskkill /PID $pid_ /T /F | Out-Null
    Remove-Item $PidFile -ErrorAction SilentlyContinue
    Write-Output '已停'
  }
  'status' {
    $pid_ = Get-WatchPid
    if ($pid_) { Write-Output "运行中（pid $pid_）" } else { Write-Output '没在跑'; exit 1 }
  }
  'logs' {
    if (-not (Test-Path $LogFile)) { Write-Error "还没有日志：$LogFile" }
    Get-Content $LogFile -Encoding UTF8 -Tail 50 -Wait
  }
  'run' {
    New-Item -ItemType Directory -Force -Path $RunDir | Out-Null
    Watch-Loop $bin
  }
  default {
    Write-Error "用法：$PSScriptRoot\watch.ps1 start|stop|status|logs|run"
  }
}
