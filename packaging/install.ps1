<#
.SYNOPSIS
    Install Bask Media Server on Windows and start it at boot.

.DESCRIPTION
    Fetches the release build for this machine, verifies it against the
    published checksums, installs it under Program Files, and registers a
    scheduled task that starts it at boot.

    A scheduled task rather than a Windows service, deliberately: the binary
    does not implement the service control protocol, and sc.exe create would
    register something the service manager then kills for not reporting in. A
    boot-triggered task is what actually works, and it survives a restart the
    same way.

    Nothing is compiled. Run it from an elevated PowerShell:

        irm https://baskmedia.app/install.ps1 | iex

    It is safe to run again: an existing configuration file is left alone and
    the installation is upgraded in place.

.PARAMETER RunAsUser
    The account the server runs as. LocalSystem by default, which can read any
    local disk. A library on a network share needs a real account instead,
    because LocalSystem has no network credentials — pass the user name and
    the task is registered to run whether or not anybody is signed in.

.PARAMETER Version
    A release tag such as v1.2.0. The latest release by default.
#>
[CmdletBinding()]
param(
    [string] $RunAsUser = 'LocalSystem',
    [string] $Version = 'latest',
    [string] $Repository = 'Ink-Iron-Apps/bask-media-server',
    [string] $InstallDir = "$env:ProgramFiles\Bask Media Server",
    [string] $DataDir = "$env:ProgramData\Bask Media Server"
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$TaskName = 'Bask Media Server'

function Say([string] $Message) { Write-Host "==> $Message" }

function Assert-Elevated {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this from an elevated PowerShell: it registers a scheduled task and writes to Program Files.'
    }
}

function Get-TargetArchitecture {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "No release build for $env:PROCESSOR_ARCHITECTURE; build from source instead." }
    }
}

function Get-LatestTag {
    Say 'finding the latest release'
    $release = Invoke-RestMethod -UseBasicParsing `
        -Uri "https://api.github.com/repos/$Repository/releases/latest"
    if (-not $release.PSObject.Properties['tag_name']) {
        throw 'Could not work out the latest release; pass -Version vX.Y.Z.'
    }
    $release.tag_name
}

Assert-Elevated

$architecture = Get-TargetArchitecture
if ($Version -eq 'latest') { $Version = Get-LatestTag }
$plain = $Version.TrimStart('v')

$archive = "bms_${plain}_windows_${architecture}.zip"
$base = "https://github.com/$Repository/releases/download/$Version"
$work = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "bms-install-$([guid]::NewGuid())")

try {
    Say "downloading $archive"
    $zip = Join-Path $work $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zip
    $sums = Join-Path $work 'SHA256SUMS'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums

    # Verify before unpacking. The checksum line for this archive has to be
    # present: a missing line is a failure, not a pass.
    Say 'checking the download'
    $expected = $null
    foreach ($line in Get-Content $sums) {
        $fields = $line.Trim() -split '\s+', 2
        if ($fields.Count -eq 2 -and $fields[1].Trim() -eq $archive) {
            $expected = $fields[0]
            break
        }
    }
    if (-not $expected) { throw "$archive is not in the published checksums." }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash
    if ($actual -ne $expected.ToUpperInvariant()) {
        throw 'The download does not match its checksum.'
    }

    Expand-Archive -Path $zip -DestinationPath (Join-Path $work 'unpacked') -Force
    $unpacked = Join-Path $work 'unpacked'
    $binary = Join-Path $unpacked 'bms.exe'
    if (-not (Test-Path $binary)) { throw 'The archive does not contain bms.exe.' }

    # Stop a running copy so the binary can be replaced. A task that is not
    # there yet is not an error.
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Say 'stopping the running server'
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        Get-Process -Name 'bms' -ErrorAction SilentlyContinue |
            Where-Object { $_.Path -like "$InstallDir*" } |
            Stop-Process -Force
        Start-Sleep -Seconds 2
    }

    Say "installing to $InstallDir"
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
    Copy-Item -Path $binary -Destination (Join-Path $InstallDir 'bms.exe') -Force
    foreach ($extra in 'README.md', 'LICENSE') {
        $path = Join-Path $unpacked $extra
        if (Test-Path $path) { Copy-Item -Path $path -Destination $InstallDir -Force }
    }

    $configuration = Join-Path $DataDir 'bms.conf'
    $sample = Join-Path $unpacked 'packaging\bms.conf.example'
    if (Test-Path $configuration) {
        Say "keeping the configuration already at $configuration"
        Copy-Item -Path $sample -Destination (Join-Path $DataDir 'bms.conf.example') -Force
    }
    else {
        Say "writing $configuration"
        # The sample's paths are the Linux ones, so rewrite the two that would
        # otherwise be wrong on a Windows machine rather than leaving somebody
        # to notice.
        (Get-Content $sample -Raw).
            Replace('/var/lib/bask-media-server/bms.db', (Join-Path $DataDir 'bms.db')).
            Replace('/mnt/media/', 'D:\Media\').
            Replace('/mnt/media', 'D:\Media') |
            Set-Content -Path $configuration -Encoding UTF8
    }

    Say "registering the $TaskName task"
    $action = New-ScheduledTaskAction -Execute (Join-Path $InstallDir 'bms.exe') `
        -Argument "-config `"$configuration`"" -WorkingDirectory $InstallDir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 3 `
        -RestartInterval (New-TimeSpan -Minutes 1) `
        -ExecutionTimeLimit (New-TimeSpan -Seconds 0)

    if ($RunAsUser -eq 'LocalSystem') {
        $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' `
            -LogonType ServiceAccount -RunLevel Highest
    }
    else {
        # S4U runs the task as the named account without storing its password,
        # whether or not that account is signed in. It has no network
        # credentials either, so a network share still needs a stored password
        # -- which is a choice for whoever owns the account, not this script.
        $principal = New-ScheduledTaskPrincipal -UserId $RunAsUser `
            -LogonType S4U -RunLevel Highest
    }

    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Settings $settings -Principal $principal -Force | Out-Null

    Write-Host ''
    Say "Bask Media Server $Version is installed."
    Write-Host @"

Next, the part only you can decide: edit

    $configuration

and add your libraries. A library line looks like

    section = Movies:movies:D:\Media\Movies

Then start it and read the token it prints on the first run:

    Start-ScheduledTask -TaskName '$TaskName'

The first start writes the admin token to the log. Set a log path in the
configuration file first if you want to read it back later:

    log = $DataDir\server.log

The admin interface is at http://<this machine>:32500/admin.

"@
}
finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
