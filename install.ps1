# Installs even-cc on Windows from the latest GitHub release:
#
#   irm https://raw.githubusercontent.com/sorbet-ai/even-cc/main/install.ps1 | iex
#
# Environment: EVEN_CC_VERSION=v1.2.3 (default: latest),
# EVEN_CC_INSTALL_DIR (default: %LOCALAPPDATA%\Programs\even-cc),
# EVEN_CC_NO_PATH=1 (don't add the folder to your PATH).
$ErrorActionPreference = 'Stop'

$repo = 'sorbet-ai/even-cc'
$version = if ($env:EVEN_CC_VERSION) { $env:EVEN_CC_VERSION } else { 'latest' }
$dir = if ($env:EVEN_CC_INSTALL_DIR) { $env:EVEN_CC_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\even-cc' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "even-cc_windows_$arch.zip"
$base = if ($env:EVEN_CC_BASE_URL) { $env:EVEN_CC_BASE_URL } elseif ($version -eq 'latest') { "https://github.com/$repo/releases/latest/download" } else { "https://github.com/$repo/releases/download/$version" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("even-cc-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $asset ($version)..."
  Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile (Join-Path $tmp $asset)
  $sums = Join-Path $tmp 'checksums.txt'
  try { Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile $sums } catch { $sums = $null }
  if ($sums) {
    $want = ((Get-Content $sums | Where-Object { $_ -match "[ *]$([regex]::Escape($asset))$" }) -split ' ')[0]
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash.ToLower()
    if (-not $want -or $want -ne $got) { throw "checksum mismatch for $asset" }
  }
  Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath (Join-Path $tmp 'x') -Force
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $exe = Join-Path $dir 'even-cc.exe'
  # a running even-cc.exe can't be overwritten, but it can be renamed
  if (Test-Path $exe) {
    Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
    Rename-Item $exe 'even-cc.exe.old' -ErrorAction SilentlyContinue
  }
  Copy-Item (Join-Path $tmp 'x\even-cc.exe') $exe -Force
  Write-Host "Installed $(& $exe version) to $exe"

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (-not $env:EVEN_CC_NO_PATH -and -not (($userPath -split ';') -contains $dir)) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $dir), 'User')
    $env:Path += ";$dir"
    Write-Host "Added $dir to your PATH (new terminals pick it up)."
  }
  if (-not (Get-Command claude -ErrorAction SilentlyContinue)) {
    Write-Host "Note: Claude Code (claude) isn't on your PATH; even-cc needs it: https://claude.com/claude-code"
  }
  Write-Host ""
  Write-Host "Next: run 'even-cc start' and scan the QR code with Faceclaw"
  Write-Host "(Claude Code > Settings > Machines > Add machine). Stop it with 'even-cc stop'."
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
