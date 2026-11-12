# Regenerate Go and Python code from proto/. Both compilers run in Docker.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$module = 'github.com/neurohalo/neurohalo'

& "$PSScriptRoot/go.ps1" sh -c "protoc -I proto --go_out=. --go_opt=module=$module --go-grpc_out=. --go-grpc_opt=module=$module proto/neuro/v1/*.proto"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

# Python's protoc also runs in Docker: this machine's application control
# policy blocks the compiler's DLL on the host. The version matches uv.lock.
docker run --rm -v "${root}:/src" -w /src/replayer ghcr.io/astral-sh/uv:python3.12-bookworm-slim sh -c "uv run --no-project --with grpcio-tools==1.85.1 python -m grpc_tools.protoc -I ../proto --python_out=. --grpc_python_out=. neuro/v1/*.proto"
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
"generated: gen/neuro/v1 (Go), replayer/neuro/v1 (Python)"
