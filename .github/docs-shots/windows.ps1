# Takes the Windows docs screenshots on a GitHub-hosted runner: runs the
# real one-line installer and the agent's commands in a visible PowerShell
# console, opens the status page in Edge, and captures each window.
#
# Needs a Logbook server on LOGBOOK_SERVER and a key in LOGBOOK_API_KEY
# (set by the workflow; the key never appears on screen). Writes
# windows-<step>.png and windows-<step>.txt to OutDir.
#
# This file must stay ASCII: Windows PowerShell 5.1 reads BOM-less scripts
# in the legacy code page.
param([Parameter(Mandatory)][string]$OutDir)

$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$work = Join-Path $env:RUNNER_TEMP 'shots-work'
New-Item -ItemType Directory -Force -Path $work | Out-Null

Add-Type -AssemblyName System.Drawing
Add-Type @'
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class Win {
    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left, Top, Right, Bottom; }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    public struct DEVMODE {
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 32)] public string dmDeviceName;
        public short dmSpecVersion, dmDriverVersion, dmSize, dmDriverExtra;
        public int dmFields, dmPositionX, dmPositionY, dmDisplayOrientation, dmDisplayFixedOutput;
        public short dmColor, dmDuplex, dmYResolution, dmTTOption, dmCollate;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 32)] public string dmFormName;
        public short dmLogPixels;
        public int dmBitsPerPel, dmPelsWidth, dmPelsHeight, dmDisplayFlags, dmDisplayFrequency;
        public int dmICMMethod, dmICMIntent, dmMediaType, dmDitherType, dmReserved1, dmReserved2;
        public int dmPanningWidth, dmPanningHeight;
    }

    public delegate bool EnumProc(IntPtr hwnd, IntPtr lParam);

    [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
    [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr lParam);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr hwnd);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int GetClassName(IntPtr hwnd, StringBuilder s, int n);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int GetWindowText(IntPtr hwnd, StringBuilder s, int n);
    [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr hwnd, out uint pid);
    [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hwnd);
    [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr hwnd, int cmd);
    [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr hwnd, IntPtr after, int x, int y, int w, int h, uint flags);
    [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr hwnd, uint msg, IntPtr w, IntPtr l);
    [DllImport("user32.dll")] public static extern int GetSystemMetrics(int i);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern bool EnumDisplaySettings(string dev, int mode, ref DEVMODE dm);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int ChangeDisplaySettings(ref DEVMODE dm, int flags);
    [DllImport("dwmapi.dll")] public static extern int DwmGetWindowAttribute(IntPtr hwnd, int attr, out RECT r, int size);

    public static string ClassOf(IntPtr h) { var s = new StringBuilder(256); GetClassName(h, s, 256); return s.ToString(); }
    public static string TitleOf(IntPtr h) { var s = new StringBuilder(512); GetWindowText(h, s, 512); return s.ToString(); }

    public static IntPtr[] Visible() {
        var list = new System.Collections.Generic.List<IntPtr>();
        EnumWindows((h, l) => { if (IsWindowVisible(h)) list.Add(h); return true; }, IntPtr.Zero);
        return list.ToArray();
    }

    // Bounds without the invisible resize border Windows 10+ adds.
    public static RECT Bounds(IntPtr h) {
        RECT r;
        DwmGetWindowAttribute(h, 9, out r, Marshal.SizeOf(typeof(RECT)));
        return r;
    }

    public static int SetResolution(int w, int h) {
        var dm = new DEVMODE();
        dm.dmSize = (short)Marshal.SizeOf(typeof(DEVMODE));
        if (!EnumDisplaySettings(null, -1, ref dm)) return -100;
        dm.dmPelsWidth = w; dm.dmPelsHeight = h;
        dm.dmFields = 0x80000 | 0x100000;
        return ChangeDisplaySettings(ref dm, 0);
    }
}
'@
[Win]::SetProcessDPIAware() | Out-Null

function Log([string]$msg) { Write-Host "[shots] $msg" }

# --- Desktop setup ---------------------------------------------------------

$res = [Win]::SetResolution(1920, 1080)
Log "set resolution 1920x1080: result $res, screen now $([Win]::GetSystemMetrics(0))x$([Win]::GetSystemMetrics(1))"

# Console defaults: classic conhost, Consolas at a readable size, ~1000x600.
$console = 'HKCU:\Console'
New-Item -Force -Path $console | Out-Null
Set-ItemProperty $console FaceName 'Consolas'
Set-ItemProperty $console FontFamily 0x36 -Type DWord
Set-ItemProperty $console FontWeight 400 -Type DWord
Set-ItemProperty $console FontSize 0x00160000 -Type DWord      # 22 px tall (about 16.5 pt)
Set-ItemProperty $console WindowSize 0x001A005A -Type DWord    # 90 x 26
Set-ItemProperty $console ScreenBufferSize 0x0BB8005A -Type DWord  # 90 x 3000
Set-ItemProperty $console WindowPosition 0x00280028 -Type DWord   # 40, 40
Set-ItemProperty $console QuickEdit 0 -Type DWord
# Blue Windows PowerShell background, as on a fresh install.
Set-ItemProperty $console ScreenColors 0x56 -Type DWord
Set-ItemProperty $console ColorTable05 0x00562401 -Type DWord
Set-ItemProperty $console ColorTable06 0x00f0edee -Type DWord
# Per-shortcut settings would override the defaults above.
Get-ChildItem $console -ErrorAction SilentlyContinue | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
# Open console programs in conhost, not Windows Terminal.
New-Item -Force -Path 'HKCU:\Console\%%Startup' | Out-Null
Set-ItemProperty 'HKCU:\Console\%%Startup' DelegationConsole '{B23D10C0-E52E-411E-9D5B-C09FDF709C7D}'
Set-ItemProperty 'HKCU:\Console\%%Startup' DelegationTerminal '{B23D10C0-E52E-411E-9D5B-C09FDF709C7D}'

# Edge without first-run pages, sign-in prompts or default-browser bars.
$edge = 'HKLM:\SOFTWARE\Policies\Microsoft\Edge'
New-Item -Force -Path $edge | Out-Null
foreach ($p in @{
        HideFirstRunExperience = 1; BrowserSignin = 0; SyncDisabled = 1; DefaultBrowserSettingEnabled = 0
        DefaultBrowserSettingsCampaignEnabled = 0; ShowRecommendationsEnabled = 0; PromotionalTabsEnabled = 0
        StartupBoostEnabled = 0; HubsSidebarEnabled = 0; EdgeShoppingAssistantEnabled = 0
        PersonalizationReportingEnabled = 0; SpotlightExperiencesAndRecommendationsEnabled = 0
        NewTabPageContentEnabled = 0; ShowMicrosoftRewards = 0; AutoImportAtFirstRun = 4
        ImportOnEachLaunch = 0; WalletDonationEnabled = 0; CopilotPageContext = 0; EdgeCollectionsEnabled = 0
        BackgroundModeEnabled = 0; RestoreOnStartup = 5; TranslateEnabled = 0; PasswordManagerEnabled = 0
    }.GetEnumerator()) {
    Set-ItemProperty $edge $p.Key $p.Value -Type DWord
}

# Close windows that could cover ours (Server Manager opens at logon).
Get-Process ServerManager -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

# --- Capture helpers -------------------------------------------------------

function Save-Window([IntPtr]$hwnd, [string]$path) {
    [Win]::ShowWindow($hwnd, 9) | Out-Null   # restore
    $top = [IntPtr](-1); $notop = [IntPtr](-2)
    [Win]::SetWindowPos($hwnd, $top, 0, 0, 0, 0, 0x0003) | Out-Null   # NOSIZE|NOMOVE
    [Win]::SetForegroundWindow($hwnd) | Out-Null
    Start-Sleep -Milliseconds 800
    $r = [Win]::Bounds($hwnd)
    $w = $r.Right - $r.Left; $h = $r.Bottom - $r.Top
    Log "capturing $path at $($r.Left),$($r.Top) ${w}x$h"
    $bmp = New-Object System.Drawing.Bitmap $w, $h
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.CopyFromScreen($r.Left, $r.Top, 0, 0, $bmp.Size)
    $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
    $g.Dispose(); $bmp.Dispose()
    [Win]::SetWindowPos($hwnd, $notop, 0, 0, 0, 0, 0x0003) | Out-Null
}

function Save-Screen([string]$path) {
    $w = [Win]::GetSystemMetrics(0); $h = [Win]::GetSystemMetrics(1)
    $bmp = New-Object System.Drawing.Bitmap $w, $h
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.CopyFromScreen(0, 0, 0, 0, $bmp.Size)
    $bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
    $g.Dispose(); $bmp.Dispose()
}

function Find-Window([scriptblock]$match, [int]$timeoutSec = 30) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        foreach ($h in [Win]::Visible()) {
            $cls = [Win]::ClassOf($h); $title = [Win]::TitleOf($h)
            if (& $match $cls $title) { return $h }
        }
        Start-Sleep -Milliseconds 500
    }
    Log 'visible windows:'
    foreach ($h in [Win]::Visible()) { Log "  [$([Win]::ClassOf($h))] $([Win]::TitleOf($h))" }
    throw 'window not found'
}

$stepScript = Join-Path $PSScriptRoot 'console-step.ps1'

# Runs one command in a new visible console and captures it.
function Invoke-ConsoleShot([string]$name, [string]$command, [int]$timeoutSec = 240) {
    $cmdFile = Join-Path $work "$name.cmd.txt"
    $done = Join-Path $work "$name.done"
    $txt = Join-Path $OutDir "windows-$name.txt"
    [IO.File]::WriteAllText($cmdFile, $command)
    Remove-Item -Force -ErrorAction SilentlyContinue $done
    $psArgs = "-NoLogo -NoExit -ExecutionPolicy Bypass -File `"$stepScript`" -CommandFile `"$cmdFile`" -TextFile `"$txt`" -DoneFile `"$done`""
    # conhost.exe runs the console itself, so it opens on the desktop even
    # when Windows Terminal is the default terminal.
    $proc = Start-Process -FilePath "$env:WINDIR\System32\conhost.exe" -ArgumentList "powershell.exe $psArgs" -PassThru
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while (-not (Test-Path $done)) {
        if ((Get-Date) -gt $deadline) { Save-Screen (Join-Path $OutDir "debug-$name-timeout.png"); throw "$name timed out" }
        Start-Sleep -Seconds 1
    }
    Start-Sleep -Seconds 2   # let the prompt draw
    $hwnd = Find-Window { param($c, $t) $c -eq 'ConsoleWindowClass' -and $t -like '*PowerShell*' }
    Save-Window $hwnd (Join-Path $OutDir "windows-$name.png")
    Save-Screen (Join-Path $OutDir "debug-$name-screen.png")
    Get-Content -LiteralPath $txt | ForEach-Object { Write-Host "  | $_" }
    [Win]::PostMessage($hwnd, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null   # WM_CLOSE
    Start-Sleep -Seconds 1
    if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
}

# --- The shots -------------------------------------------------------------

# This script runs in PowerShell 7, whose module path would make the
# Windows PowerShell 5.1 consoles load 7's modules (and lose Get-FileHash).
# Without the variable, 5.1 builds its own default path, as on a desktop.
Import-Module Microsoft.PowerShell.Utility, Microsoft.PowerShell.Management
Remove-Item Env:PSModulePath -ErrorAction SilentlyContinue

$installer = 'irm https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.ps1 | iex'
$exe = '& "$env:LOCALAPPDATA\Programs\Logbook\logbook.exe"'

Invoke-ConsoleShot 'install' $installer
Start-Sleep -Seconds 5   # let the daemon finish its first cycle
Invoke-ConsoleShot 'status' "$exe status"
Invoke-ConsoleShot 'doctor' "$exe doctor"

# The status page in Edge.
$cfg = Get-Content -Raw (Join-Path $env:APPDATA 'logbook\config.json') -ErrorAction SilentlyContinue | ConvertFrom-Json
$local = if ($cfg -and $cfg.local_addr) { $cfg.local_addr } else { '127.0.0.1:47231' }
$url = "http://$local/"
Log "opening $url in Edge"
$edgeExe = @("${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe", "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe") | Where-Object { Test-Path $_ } | Select-Object -First 1
$profileDir = Join-Path $work 'edge-profile'
$edgeProc = Start-Process -FilePath $edgeExe -PassThru -ArgumentList @(
    '--no-first-run', '--no-default-browser-check', '--disable-features=msEdgeSidebarV2,msUndersideButton,EdgeSidebar',
    "--user-data-dir=`"$profileDir`"", '--window-position=40,40', '--window-size=1280,900', '--new-window', $url)
Start-Sleep -Seconds 10
$hwnd = Find-Window { param($c, $t) $c -eq 'Chrome_WidgetWin_1' -and $t -like '*Edge*' } 60
Log "edge window title: $([Win]::TitleOf($hwnd))"
[Win]::SetWindowPos($hwnd, [IntPtr]::Zero, 40, 40, 1280, 900, 0x0004) | Out-Null
Start-Sleep -Seconds 3
Save-Window $hwnd (Join-Path $OutDir 'windows-status-page.png')
Save-Screen (Join-Path $OutDir 'debug-status-page-screen.png')
Get-Process msedge -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

Invoke-ConsoleShot 'uninstall' "$exe uninstall"
Log 'done'
