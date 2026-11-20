# Logbook installer for Windows (Windows PowerShell 5.1 or PowerShell 7).
# No admin rights needed: everything goes in your user profile.
#
#   $env:LOGBOOK_API_KEY = "<key>"; irm https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.ps1 | iex
#
# Or, to pass arguments to `logbook install`:
#
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.ps1))) --key <key> --harness none
#
# Passing the key in LOGBOOK_API_KEY keeps it out of the process list.
# $env:LOGBOOK_VERSION picks a release tag (default: the latest release).
#
# This file must stay ASCII: Windows PowerShell 5.1 reads BOM-less scripts
# in the legacy code page.

function Install-Logbook {
    param([string[]]$InstallArgs)

    $ErrorActionPreference = 'Stop'
    # Invoke-WebRequest's progress bar makes downloads many times slower on 5.1.
    $ProgressPreference = 'SilentlyContinue'
    # Older Windows 10 builds default to TLS 1.0; GitHub needs 1.2.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'apneduniya/logbook'

    # A 32-bit PowerShell on 64-bit Windows reports x86 here; the real
    # architecture is then in PROCESSOR_ARCHITEW6432.
    $cpu = $env:PROCESSOR_ARCHITEW6432
    if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "Unsupported CPU '$cpu': Logbook ships 64-bit builds (amd64 and arm64) only." }
    }
    $asset = "logbook_windows_$arch.exe"

    $version = $env:LOGBOOK_VERSION
    if (-not $version) { $version = 'latest' }
    if ($version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        $base = "https://github.com/$repo/releases/download/$version"
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('logbook-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $exe = Join-Path $tmp $asset
        $sums = Join-Path $tmp 'checksums.txt'
        Write-Host "Downloading Logbook ($version, windows/$arch)..."
        Invoke-WebRequest -UseBasicParsing -TimeoutSec 300 -Uri "$base/$asset" -OutFile $exe
        Invoke-WebRequest -UseBasicParsing -TimeoutSec 60 -Uri "$base/checksums.txt" -OutFile $sums

        $expected = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $parts = $line.Trim() -split '\s+'
            if ($parts.Count -ge 2 -and ($parts[1] -eq $asset -or $parts[1] -eq "*$asset")) {
                $expected = $parts[0].ToLowerInvariant()
            }
        }
        if (-not $expected) { throw "$asset is missing from checksums.txt" }
        $actual = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "Checksum mismatch for $asset (expected $expected, got $actual); not installing."
        }
        Write-Host 'Checksum verified.'

        & $exe install @InstallArgs
        if ($LASTEXITCODE -ne 0) { throw "logbook install failed (exit code $LASTEXITCODE)." }
    } finally {
        # The installer has copied itself into place; the download can go.
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# Throw rather than exit: under "irm | iex", exit would close the user's
# PowerShell window.
Install-Logbook -InstallArgs $args
