[CmdletBinding()]
param(
    [string]$GoExecutable = "go",
    [switch]$Race
)

$ErrorActionPreference = "Stop"
$go = (Get-Command $GoExecutable -CommandType Application -ErrorAction Stop).Source
$gofmtName = if ($IsWindows) { "gofmt.exe" } else { "gofmt" }
$gofmt = Join-Path (Split-Path -Parent $go) $gofmtName
$stamp = [DateTime]::UtcNow.ToString("yyyyMMddTHHmmssZ")
$runName = "run-$stamp-$([Guid]::NewGuid().ToString('N').Substring(0, 8))"
$artifacts = Join-Path $PSScriptRoot (Join-Path "artifacts" $runName)
New-Item -ItemType Directory -Path $artifacts | Out-Null
$oldEnvironment = @{
    GOTOOLCHAIN = $env:GOTOOLCHAIN
    GOTELEMETRY = $env:GOTELEMETRY
    CGO_ENABLED = $env:CGO_ENABLED
    METADATA_SERVER_BIN = $env:METADATA_SERVER_BIN
    E2E_ARTIFACTS_DIR = $env:E2E_ARTIFACTS_DIR
}
$status = "failed"
$phase = "setup"
$started = [DateTime]::UtcNow

function Invoke-Go {
    param([string[]]$Arguments, [string]$Log)
    & $go @Arguments 2>&1 | Tee-Object -FilePath (Join-Path $artifacts $Log)
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
}

Push-Location $PSScriptRoot
try {
    $env:GOTOOLCHAIN = "local"
    $env:GOTELEMETRY = "off"
    $env:CGO_ENABLED = "0"
    $version = & $go version
    if ($LASTEXITCODE -ne 0) { throw "Cannot execute Go." }
    $phase = "format"
    $files = @(Get-ChildItem -LiteralPath "cmd", "internal", "e2e" -Filter "*.go" -Recurse -File |
        Sort-Object FullName | ForEach-Object FullName)
    $unformatted = @(& $gofmt -l @files)
    if ($LASTEXITCODE -ne 0) { throw "gofmt failed." }
    if ($unformatted.Count -ne 0) {
        $unformatted | Set-Content -LiteralPath (Join-Path $artifacts "unformatted.txt")
        throw "Go files need formatting. See $artifacts."
    }
    $phase = "dependencies"
    Invoke-Go -Arguments @("mod", "verify") -Log "dependencies.txt"
    $phase = "vet"
    Invoke-Go -Arguments @("vet", (Join-Path "." "...")) -Log "vet.txt"
    $phase = "unit"
    $coverage = Join-Path $artifacts "coverage.out"
    $corePackages = @((Join-Path "." "internal\..."), (Join-Path "." "cmd\..."))
    $dependencies = @(& $go list -deps (Join-Path "." "cmd\..."))
    if ($LASTEXITCODE -ne 0) { throw "Cannot identify executable packages for coverage." }
    $productionPackages = @($dependencies | Where-Object { $_.StartsWith("metadata-api/") })
    if ($productionPackages.Count -eq 0) { throw "No executable packages were found for coverage." }
    $productionPackages | Set-Content -LiteralPath (Join-Path $artifacts "coverage-packages.txt")
    $coverPackages = $productionPackages -join ","
    Invoke-Go -Arguments (@("test", "-count=1", "-shuffle=on", "-coverpkg=$coverPackages", "-coverprofile=$coverage") + $corePackages) -Log "unit.txt"
    Invoke-Go -Arguments @("tool", "cover", "-func=$coverage") -Log "coverage-summary.txt"
    $phase = "build"
    $bin = Join-Path $PSScriptRoot "bin"
    New-Item -ItemType Directory -Path $bin -Force | Out-Null
    $binaryName = if ($IsWindows) { "metadata-server.exe" } else { "metadata-server" }
    $binary = Join-Path $bin $binaryName
    Invoke-Go -Arguments @("build", "-o", $binary, (Join-Path "." "cmd\metadata-server")) -Log "build.txt"
    $phase = "e2e"
    $env:METADATA_SERVER_BIN = $binary
    $env:E2E_ARTIFACTS_DIR = Join-Path $artifacts "e2e"
    Invoke-Go -Arguments @("test", "-count=1", "-timeout=3m", "-v", (Join-Path "." "e2e")) -Log "e2e.txt"
    if ($Race) {
        $phase = "race"
        $env:CGO_ENABLED = "1"
        Invoke-Go -Arguments (@("test", "-race", "-count=1", "-shuffle=on") + $corePackages) -Log "race.txt"
    }
    $status = "passed"
    $phase = "complete"
}
finally {
    @{
        status = $status
        phase = $phase
        started_at = $started.ToString("o")
        completed_at = [DateTime]::UtcNow.ToString("o")
        go = $version
        race_requested = [bool]$Race
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $artifacts "result.json") -Encoding utf8
    foreach ($name in $oldEnvironment.Keys) {
        Set-Item -Path "Env:$name" -Value $oldEnvironment[$name]
    }
    Pop-Location
    Write-Host "Verification $status. Evidence is in $artifacts."
}
