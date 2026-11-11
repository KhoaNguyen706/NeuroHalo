# Run the stack on Kubernetes, in a local kind cluster, with the Helm chart
# in deploy/helm/neurohalo.
#   scripts/k8s.ps1 up      # create the cluster, build and load the images, install the chart
#   scripts/k8s.ps1 demo    # play 6 sessions, kill a processor pod mid-way, report what was stored
#   scripts/k8s.ps1 down    # delete the cluster and start Docker Compose again
# The cluster uses the same localhost ports as Docker Compose (3000, 8080,
# 50051), so "up" stops the Compose stack first. kind and helm are downloaded
# into bin/ (not committed) when they are not installed.
param([Parameter(Mandatory)][ValidateSet('up', 'demo', 'down')][string]$Action)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$bin = "$root/bin"
$cluster = 'neurohalo'
$context = "kind-$cluster"
$images = 'gateway', 'processor', 'api', 'worker', 'web'
$api = @{ Authorization = 'Bearer dev-token' }

# Return the path of kind or helm: the installed one, or a copy downloaded into bin/.
function Tool([string]$name) {
    $installed = Get-Command $name -ErrorAction SilentlyContinue
    if ($installed) { return $installed.Source }
    $exe = "$bin/$name.exe"
    if (Test-Path $exe) { return $exe }
    New-Item -ItemType Directory -Force $bin | Out-Null
    Write-Host "downloading $name into bin/"
    $ProgressPreference = 'SilentlyContinue'
    if ($name -eq 'kind') {
        Invoke-WebRequest 'https://kind.sigs.k8s.io/dl/v0.27.0/kind-windows-amd64' -OutFile $exe
    } else {
        Invoke-WebRequest 'https://get.helm.sh/helm-v3.17.0-windows-amd64.zip' -OutFile "$bin/helm.zip"
        Expand-Archive "$bin/helm.zip" "$bin/helm-unzip" -Force
        Move-Item "$bin/helm-unzip/windows-amd64/helm.exe" $exe -Force
        Remove-Item "$bin/helm.zip", "$bin/helm-unzip" -Recurse -Force
    }
    return $exe
}

# Run a native command and stop the script when it fails.
function Run([string]$exe) {
    & $exe @args
    if ($LASTEXITCODE -ne 0) { throw "$exe $($args -join ' ') failed" }
}

# Run SQL in the cluster's Postgres and return the rows as text, fields separated by "|".
function Sql([string]$query) {
    kubectl --context $context exec deploy/postgres -- psql -U neuro -d neuro -At -c $query
}

if ($Action -eq 'up') {
    $kind = Tool kind
    $helm = Tool helm
    docker compose -f "$root/docker-compose.yml" stop | Out-Null   # frees ports 3000, 8080 and 50051
    if (-not ((& $kind get clusters) -contains $cluster)) {
        Run $kind create cluster --config "$root/deploy/kind.yaml"
    }
    # Attestations are extra manifests that "kind load" cannot import.
    $env:BUILDX_NO_DEFAULT_ATTESTATIONS = '1'
    Run docker compose -f "$root/docker-compose.yml" build @images
    foreach ($i in $images) { Run docker tag "neurohalo-${i}:latest" "neurohalo-${i}:dev" }
    Run $kind load docker-image --name $cluster @($images | ForEach-Object { "neurohalo-${_}:dev" })
    kubectl --context $context create configmap db-schema --from-file="$root/db/schema.sql" --dry-run=client -o yaml |
        kubectl --context $context apply -f - | Out-Null
    $installed = (& $helm list --kube-context $context -q) -contains 'neurohalo'
    Run $helm upgrade --install neurohalo "$root/deploy/helm/neurohalo" --kube-context $context
    # The image tag never changes, so on a second "up" the pods are restarted to pick up rebuilt images.
    if ($installed) {
        Run kubectl --context $context rollout restart deployment @images
        foreach ($i in $images) { Run kubectl --context $context rollout status "deployment/$i" --timeout=300s }
        Start-Sleep -Seconds 35   # an old worker pod can still claim jobs until its shutdown time is over
    }
    Write-Host 'waiting for the pods (the first start pulls Kafka and Postgres)'
    Run kubectl --context $context wait --for=condition=Available deployment --all --timeout=600s
    kubectl --context $context get pods
    Write-Host 'ready: http://localhost:3000'
}

if ($Action -eq 'demo') {
    Sql "SELECT 1" | Out-Null
    Write-Host 'starting 6 sessions (eyes closed, 61 s each)'
    $ids = 1..6 | ForEach-Object { (Invoke-RestMethod 'http://localhost:8080/samples/eyes-closed/play?speed=1x' -Method Post -Headers $api).sessionId }
    Start-Sleep -Seconds 30   # the first play downloads the sample; then every session is streaming
    $live = (Sql "SELECT count(*) FROM sessions WHERE status = 'live'")
    $victim = (kubectl --context $context get pods -l app=processor -o name)[0]
    Write-Host "$live sessions live; killing $victim"
    # cmd swallows kubectl's warning about forced deletion, which PowerShell would treat as an error.
    cmd /c "kubectl --context $context delete $victim --grace-period=0 --force >nul 2>&1"
    $killed = Get-Date
    Start-Sleep -Seconds 5
    kubectl --context $context get pods -l app=processor
    Write-Host 'waiting for the sessions to end'
    $deadline = (Get-Date).AddSeconds(150)
    while ((Get-Date) -lt $deadline -and [int](Sql "SELECT count(*) FROM sessions WHERE status = 'live'") -gt 0) { Start-Sleep -Seconds 5 }
    Start-Sleep -Seconds 15   # the api writes a session's last 10-second bucket a few seconds after it ends

    # A 61-second session gives 119 half-second frames when nothing is lost (the first 2 seconds fill the buffer).
    $list = ($ids | ForEach-Object { "'$_'" }) -join ','
    Sql "SELECT s.id, s.status, COALESCE(sum(r.frames), 0), count(r.*) FROM sessions s LEFT JOIN rollups r ON r.session_id = s.id WHERE s.id IN ($list) GROUP BY s.id, s.status ORDER BY s.id" |
        ForEach-Object {
            $f = $_ -split '\|'
            [pscustomobject]@{ Session = $f[0]; Status = $f[1]; 'Frames stored' = [int]$f[2]; 'Of 119' = '{0:P0}' -f ([int]$f[2] / 119); 'Buckets' = [int]$f[3] }
        } | Format-Table -AutoSize
    kubectl --context $context get pods -l app=processor
    Write-Host "processor killed at $($killed.ToString('HH:mm:ss'))"
}

if ($Action -eq 'down') {
    Run (Tool kind) delete cluster --name $cluster
    docker compose -f "$root/docker-compose.yml" start | Out-Null
    Write-Host 'cluster deleted, Docker Compose stack started again'
}
