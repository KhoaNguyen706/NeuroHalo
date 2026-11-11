# Run npm/npx in a Node container with web/ mounted, so nothing Node-related
# has to work on the host.
#   scripts/node.ps1 npm install
#   scripts/node.ps1 npm run build
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
docker run --rm -v "${root}/web:/app" -v neurohalo-npmcache:/root/.npm -w /app node:24-bookworm-slim @args
exit $LASTEXITCODE
