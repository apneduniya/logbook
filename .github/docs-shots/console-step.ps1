# Runs one command in a visible Windows PowerShell console for a docs
# screenshot. windows.ps1 opens this in conhost with -NoExit, so after the
# command the console shows a normal prompt, as it would for a user.
#
# The command is typed out after a prompt exactly as it is then run, and
# the console's text is saved to TextFile for writers to quote. DoneFile
# tells windows.ps1 the output has finished.
#
# This file must stay ASCII: Windows PowerShell 5.1 reads BOM-less scripts
# in the legacy code page.
param(
    [Parameter(Mandatory)][string]$CommandFile,
    [Parameter(Mandatory)][string]$TextFile,
    [Parameter(Mandatory)][string]$DoneFile
)

$Host.UI.RawUI.WindowTitle = 'Windows PowerShell'
Set-Location $HOME
$command = (Get-Content -Raw -LiteralPath $CommandFile).Trim()
Clear-Host
Write-Host -NoNewline ('PS ' + (Get-Location).Path + '> ')
Write-Host $command
try {
    Invoke-Expression $command
} catch {
    Write-Host ($_ | Out-String) -ForegroundColor Red
}

# Save what the console shows. A line that fills the whole width wrapped
# onto the next one, so it is joined back up.
$raw = $Host.UI.RawUI
$width = $raw.BufferSize.Width
$last = $raw.CursorPosition.Y
if ($raw.CursorPosition.X -eq 0) { $last-- }
$rect = New-Object System.Management.Automation.Host.Rectangle 0, 0, ($width - 1), $last
$cells = $raw.GetBufferContents($rect)
$lines = New-Object System.Collections.Generic.List[string]
$joined = ''
for ($row = 0; $row -le $cells.GetUpperBound(0); $row++) {
    $chars = New-Object char[] $width
    for ($col = 0; $col -lt $width; $col++) { $chars[$col] = $cells[$row, $col].Character }
    $line = New-Object string (, $chars)
    if ($line.TrimEnd().Length -eq $width) {
        $joined += $line
        continue
    }
    $lines.Add(($joined + $line).TrimEnd())
    $joined = ''
}
if ($joined) { $lines.Add($joined.TrimEnd()) }
[IO.File]::WriteAllLines($TextFile, $lines)
New-Item -ItemType File -Force -Path $DoneFile | Out-Null
