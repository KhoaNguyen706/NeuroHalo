# Run the load generator at each session count and print the numbers
# Prometheus measured during the steady part of each run.
#   scripts/loadtest.ps1                      # 10, 100, 500, 1000 sessions, 60 s each
#   scripts/loadtest.ps1 -Sessions 50 -Seconds 30
# Needs the stack up (docker compose up -d) and data/loadgen/ (replayer/make_loadgen_data.py).
param(
    [int[]]$Sessions = @(10, 100, 500, 1000),
    [int]$Seconds = 60,
    [int]$RampSeconds = 10
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$window = "$([Math]::Max(15, $Seconds - 15))s"   # leaves out the ramp and the first seconds after it

# Ask Prometheus for one number at a moment in time; $null when it has no data.
function Query([string]$expr, [double]$at) {
    $r = Invoke-RestMethod "http://localhost:9090/api/v1/query" -Method Post -Body @{ query = $expr; time = $at }
    if ($r.data.result.Count -eq 0) { return $null }
    $v = [double]$r.data.result[0].value[1]
    if ([double]::IsNaN($v)) { return $null }
    return $v
}

$rows = foreach ($n in $Sessions) {
    Write-Host "== $n sessions =="
    docker compose -f "$root/docker-compose.yml" --profile tools run --rm loadgen -sessions $n -duration "${Seconds}s" -ramp "${RampSeconds}s"
    $at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - 3   # just before the streams closed
    $p = { param($q) Query "histogram_quantile($q, sum by (le) (rate(pipeline_latency_seconds_bucket[$window])))" $at }
    [pscustomobject]@{
        Sessions     = $n
        'Batches/s'  = [Math]::Round((Query "rate(ingest_batches_total[$window])" $at))
        'MB/s'       = [Math]::Round((Query "rate(ingest_bytes_total[$window])" $at) / 1e6, 2)
        'p50 ms'     = [Math]::Round((& $p 0.5) * 1000, 1)
        'p99 ms'     = [Math]::Round((& $p 0.99) * 1000, 1)
        'Max lag'    = Query "max_over_time(sum(kafka_consumergroup_lag{consumergroup=`"processor`"})[${window}:5s])" $at
        'Gaps'       = [Math]::Round((Query "increase(missing_batches_total[$window])" $at))
    }
    Start-Sleep -Seconds 15   # let the pipeline drain before the next run
}
$rows | Format-Table -AutoSize

# Remove the test sessions and what was stored for them, once the last rollups are written.
Start-Sleep -Seconds 10
$sql = "DELETE FROM events WHERE session_id IN (SELECT id FROM sessions WHERE source = 'loadgen');" +
       "DELETE FROM rollups WHERE session_id IN (SELECT id FROM sessions WHERE source = 'loadgen');" +
       "DELETE FROM sessions WHERE source = 'loadgen';"
docker compose -f "$root/docker-compose.yml" exec -T postgres psql -q -U neuro -d neuro -c $sql
