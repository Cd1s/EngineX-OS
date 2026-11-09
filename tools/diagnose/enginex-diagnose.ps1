# EngineX OS one-click diagnostics for Windows (read-only).
#   powershell -ExecutionPolicy Bypass -File .\enginex-diagnose.ps1 [-Deck 192.168.x.y]
# Collects what we usually have to ask for one question at a time: Windows version, USB devices
# of the Denon / Numark / Rane / Rockchip ids and the driver bound to them, fastboot answers,
# usbipd (WSL) devices, the host network and, with -Deck, whether the deck answers on ports
# 22 / 80 / 8080. It never writes to the deck, never flashes, erases or reboots anything, and
# masks USB serial numbers. The report is written next to the script as enginex-diagnose-<time>.txt;
# please read it, then attach it to the GitHub issue.
param([string]$Deck = "")
$ErrorActionPreference = "Continue"
if ($Deck -and $Deck -notmatch '^[A-Za-z0-9.:-]+$') { Write-Host "Deck address: letters, digits, . : - only"; exit 2 }
$out = Join-Path (Get-Location) ("enginex-diagnose-" + (Get-Date -Format "yyyyMMdd-HHmmss") + ".txt")
$lines = New-Object System.Collections.Generic.List[string]
function Add-Line([string]$s) { $lines.Add($s) }
function Add-Section([string]$s) { $lines.Add(""); $lines.Add("## " + $s) }
function Try-Run([scriptblock]$b) { try { & $b } catch { Add-Line ("  (failed: " + $_.Exception.Message + ")") } }

Add-Line "EngineX OS diagnostics v1 (read-only)"
Add-Line ("time (UTC): " + (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ"))

Add-Section "Host"
Try-Run {
    $os = Get-CimInstance Win32_OperatingSystem
    Add-Line ($os.Caption + " " + $os.Version + " (" + $os.OSArchitecture + "), UI language " + (Get-Culture).Name)
    Add-Line ("PowerShell " + $PSVersionTable.PSVersion)
}

Add-Section "USB devices (Denon / Numark / Rane / Rockchip / fastboot ids only)"
Try-Run {
    $devs = Get-PnpDevice -PresentOnly -ErrorAction SilentlyContinue | Where-Object {
        $_.InstanceId -match 'VID_(15E4|1CC5|2207|18D1)' -or $_.FriendlyName -match 'denon|numark|rane|inmusic|mixstream|prime|engine'
    }
    if (-not $devs) { Add-Line "none found: the deck is not enumerated (cable, port, or not in Firmware Update Mode)" }
    foreach ($d in $devs) {
        $id = $d.InstanceId -replace '^([^\\]+\\[^\\]+\\).*$', '$1<serial>'   # mask the serial number
        $svc = ""
        try { $svc = (Get-PnpDeviceProperty -InstanceId $d.InstanceId -KeyName DEVPKEY_Device_Service -ErrorAction Stop).Data } catch { }
        Add-Line ("{0} | class={1} | status={2} | driver={3} | {4}" -f $id, $d.Class, $d.Status, $svc, $d.FriendlyName)
    }
}

Add-Section "fastboot"
Try-Run {
    $fb = Get-Command fastboot -ErrorAction SilentlyContinue
    if (-not $fb) { Add-Line "fastboot is not installed on Windows (it is normally run inside WSL for these decks; see the usbipd section)" }
    else {
        Add-Line ((& fastboot --version 2>&1 | Select-Object -First 1) -join "")
        $list = (& fastboot devices -l 2>&1) -join "`n"
        $list = $list -replace '(?m)^\S+', '<serial>'
        Add-Line ("devices: " + $(if ($list.Trim()) { $list } else { "none" }))
        if ($list.Trim()) {
            foreach ($v in "product", "variant", "secure", "version-bootloader", "max-download-size", "partition-type:data", "partition-size:data") {
                Add-Line ($v + ": " + ((& fastboot getvar $v 2>&1 | Select-Object -First 1) -join ""))
            }
        }
    }
}

Add-Section "usbipd / WSL"
Try-Run {
    if (Get-Command usbipd -ErrorAction SilentlyContinue) {
        (& usbipd list 2>&1) | Where-Object { $_ -match '15e4|1cc5|2207|18d1|BUSID|numark|denon|rane|mixstream' } | ForEach-Object { Add-Line $_ }
    } else { Add-Line "usbipd-win is not installed" }
    if (Get-Command wsl -ErrorAction SilentlyContinue) {
        Add-Line "wsl distributions:"
        # wsl prints UTF-16, which shows up as NUL characters here
        (& wsl -l -v 2>&1) | ForEach-Object { ("$_" -replace "`0", "").Trim() } | Where-Object { $_ } | ForEach-Object { Add-Line ("  " + $_) }
    }
}

Add-Section "Network (IPv4)"
Try-Run {
    Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.InterfaceAlias -notmatch 'Loopback' } |
        ForEach-Object { Add-Line ("{0}: {1}/{2}" -f $_.InterfaceAlias, $_.IPAddress, $_.PrefixLength) }
    Get-NetConnectionProfile -ErrorAction SilentlyContinue | ForEach-Object { Add-Line ("profile {0}: {1}" -f $_.InterfaceAlias, $_.NetworkCategory) }
}

Add-Section $(if ($Deck) { "Deck $Deck" } else { "Deck" })
if (-not $Deck) {
    Add-Line "no deck IP given; run again with -Deck 192.168.x.y  (Settings > About on the deck shows it)"
} else {
    Try-Run {
        Add-Line ("ping: " + $(if (Test-Connection -ComputerName $Deck -Count 1 -Quiet -ErrorAction SilentlyContinue) { "reply" } else { "no reply (a firewall can also cause this)" }))
        foreach ($port in 22, 80, 8080) {
            $tcp = New-Object System.Net.Sockets.TcpClient
            $ok = $false
            try { $ok = $tcp.ConnectAsync($Deck, $port).Wait(2000) -and $tcp.Connected } catch { }
            $tcp.Close()
            Add-Line ("tcp {0}: {1}" -f $port, $(if ($ok) { "open" } else { "closed or filtered" }))
        }
        foreach ($path in "/", "/v1/sc5000/ui") {
            $code = "none"; $title = ""
            try {
                $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 4 -Uri ("http://" + $Deck + ":8080" + $path)
                $code = [int]$r.StatusCode
                if ($path -eq "/" -and $r.Content -match '<title>([^<]*)</title>') { $title = $Matches[1] }
            } catch {
                if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
            }
            Add-Line ("{0} -> HTTP {1}{2}" -f $path, $code, $(if ($title) { "  title: " + $title } else { "" }))
        }
        Add-Line "(a 403 on /v1/sc5000/ui with a 'Stream Sync' title means EngineX is running and asks for its access code)"
    }
}

$lines | Out-File -FilePath $out -Encoding utf8
Write-Host ("Report written to: " + $out)
Write-Host "It holds your local IP addresses and USB ids of the deck, no serial numbers or passwords. Please attach it to the GitHub issue."
