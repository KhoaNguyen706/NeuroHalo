# Kill the recorder in the middle of a session, start it again, and check the
# session's EDF file against the original sample, sample by sample. The killed
# recorder had not committed its last Kafka offsets, so Kafka delivers those
# batches again; the merge drops the copies by seq.
#   scripts/recorder_crash.ps1                     # kill 30 s in, restart 15 s later
#   scripts/recorder_crash.ps1 -KillAfterSeconds 60
# Needs the stack up (docker compose up -d) and uv (for the MNE check).
param(
    [int]$KillAfterSeconds = 30,
    [int]$DownSeconds = 15
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$compose = @('compose', '-f', "$root/docker-compose.yml")
$api = 'http://localhost:8080'
$auth = @{ Authorization = 'Bearer dev-token' }

# The two-minute task run with markers, played in real time.
$id = (Invoke-RestMethod "$api/samples/fists/play?speed=1x" -Method Post -Headers $auth).sessionId
Write-Host "session $id started (125 s of signal)"
Start-Sleep -Seconds $KillAfterSeconds

docker @compose kill recorder | Out-Null   # SIGKILL: no last flush, no goodbye to Kafka
Write-Host "recorder killed at $(Get-Date -Format HH:mm:ss), down for $DownSeconds s"
Start-Sleep -Seconds $DownSeconds
docker @compose start recorder | Out-Null
Write-Host "recorder started at $(Get-Date -Format HH:mm:ss)"

# Wait for the session to end and its file to be written.
$deadline = (Get-Date).AddMinutes(5)
do {
    Start-Sleep -Seconds 5
    $s = Invoke-RestMethod "$api/sessions/$id" -Headers $auth
    $state = if ($s.recording) { $s.recording.status } else { 'none' }
} until (($s.status -eq 'ended' -and ($state -eq 'ready' -or $state -eq 'failed')) -or (Get-Date) -gt $deadline)
Write-Host "session $($s.status), recording $state"
if ($state -ne 'ready') { exit 1 }

docker @compose logs recorder | Select-String $id | ForEach-Object { $_.Line }
Push-Location "$root/replayer"
try { uv run check_recording.py $id ../data/S001R03.edf } finally { Pop-Location }
exit $LASTEXITCODE
