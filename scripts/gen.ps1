# Regenerate Go and Python code from proto/. Go runs in Docker, Python through uv.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$module = 'github.com/neurohalo/neurohalo'

& "$PSScriptRoot/go.ps1" sh -c "protoc -I proto --go_out=. --go_opt=module=$module --go-grpc_out=. --go-grpc_opt=module=$module proto/neuro/v1/*.proto"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Push-Location "$root/replayer"
try {
    uv run python -m grpc_tools.protoc -I ../proto --python_out=. --grpc_python_out=. (Get-ChildItem ../proto/neuro/v1/*.proto | ForEach-Object { "neuro/v1/$($_.Name)" })
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally { Pop-Location }
"generated: gen/neuro/v1 (Go), replayer/neuro/v1 (Python)"
