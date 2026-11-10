# Run any command in the Go tools container with the repo mounted.
#   scripts/go.ps1 go mod tidy
#   scripts/go.ps1 go test ./...
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
docker build -q -t neurohalo-tools -f "$root/tools/protoc.Dockerfile" "$root/tools" | Out-Null
docker run --rm -v "${root}:/src" -v neurohalo-gomod:/go/pkg/mod -v neurohalo-gocache:/root/.cache/go-build neurohalo-tools @args
exit $LASTEXITCODE
