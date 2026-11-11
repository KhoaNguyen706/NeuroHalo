# Kill one of two processors in the middle of a load test and report how the
# pipeline recovered: Kafka gives the dead processor's partitions to the
# survivor, which reads on from the last committed offset.
#   scripts/failover.ps1                 # 200 sessions, kill after 40 s
#   scripts/failover.ps1 -Sessions 50
# Needs the stack up (docker compose up -d) and data/loadgen/ (replayer/make_loadgen_data.py).
param(
    [int]$Sessions = 200,
    [int]$KillAfterSeconds = 40,
    [int]$Seconds = 100
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$compose = @('compose', '-f', "$root/docker-compose.yml")
$lag = 'sum(kafka_consumergroup_lag{consumergroup="processor"})'

# Ask Prometheus for one number at a moment in time; 0 when it has no data.
function Query([string]$expr, [double]$at) {
    $r = Invoke-RestMethod "http://localhost:9090/api/v1/query" -Method Post -Body @{ query = $expr; time = $at }
    if ($r.data.result.Count -eq 0) { return 0 }
    return [double]$r.data.result[0].value[1]
}
function Now { [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }

docker @compose up -d --scale processor=2 processor | Out-Null
Start-Sleep -Seconds 15   # both join the consumer group and share the 12 partitions
$processors = @(docker @compose ps -q processor)
if ($processors.Count -ne 2) { throw "expected 2 processors, found $($processors.Count)" }

Write-Host "== $Sessions sessions, 2 processors =="
$run = docker @compose --profile tools run -d loadgen -sessions $Sessions -duration "${Seconds}s" -ramp 10s
Start-Sleep -Seconds (10 + $KillAfterSeconds)

$before = Query 'sum(rate(pipeline_latency_seconds_count[20s]))' (Now)
$killed = Now
docker kill $processors[0] | Out-Null
Write-Host "killed one processor at $(Get-Date -Format HH:mm:ss)"
docker wait $run | Out-Null   # until the load generator has finished
$end = Now
# The load generator's own last line says how many streams failed. It logs to
# stderr, which cmd merges into the output here.
$summary = cmd /c "docker logs $run 2>&1" | Select-Object -Last 1
docker rm $run | Out-Null

# The lag every 5 seconds from the kill on: its peak, and when it was back to normal.
$r = Invoke-RestMethod "http://localhost:9090/api/v1/query_range" -Method Post -Body @{ query = $lag; start = $killed; end = $end; step = 5 }
$points = @($r.data.result[0].values | ForEach-Object { [pscustomobject]@{ t = [double]$_[0]; lag = [double]$_[1] } })
$peak = $points | Sort-Object lag -Descending | Select-Object -First 1
$normal = [Math]::Max(100, $Sessions * 2)   # a fifth of a second of batches
$back = $points | Where-Object { $_.t -gt $peak.t -and $_.lag -lt $normal } | Select-Object -First 1

[pscustomobject]@{
    'Sessions'               = $Sessions
    'Frames/s before'        = [Math]::Round($before)
    'Frames/s at the end'    = [Math]::Round((Query 'sum(rate(pipeline_latency_seconds_count[20s]))' ($end - 3)))
    'Peak lag (batches)'     = $peak.lag
    'Recovered after (s)'    = if ($back) { $back.t - $killed } else { 'not within the run' }
    'Gaps counted'           = [Math]::Round((Query "sum(increase(missing_batches_total[$($end - $killed + 10)s]))" $end))
    'Load generator'         = $summary
} | Format-List

# Back to one processor, and remove the test sessions.
docker @compose up -d --scale processor=1 processor | Out-Null
Start-Sleep -Seconds 10
$sql = "DELETE FROM events WHERE session_id IN (SELECT id FROM sessions WHERE source = 'loadgen');" +
       "DELETE FROM rollups WHERE session_id IN (SELECT id FROM sessions WHERE source = 'loadgen');" +
       "DELETE FROM sessions WHERE source = 'loadgen';"
docker @compose exec -T postgres psql -q -U neuro -d neuro -c $sql
