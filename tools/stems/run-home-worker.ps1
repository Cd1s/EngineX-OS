# Turn this Windows PC into the Stems renderer for EngineX decks on the same
# LAN / Wi-Fi. Decks find it automatically (UDP 17891 / mDNS) and render on the
# deck again once it is switched off. Set your home Wi-Fi network profile to
# Private in Windows Settings (inbound firewall rules default to Domain,Private
# + LocalSubnet; pass -AllowPublicFirewall only on a trusted home Wi-Fi marked Public).
#
#   powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1          # default: install demucs (htdemucs, NVIDIA CUDA or CPU)
#   powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 hpss     # fast spectral split, numpy + scipy only
#
# Needs Python 3 (python.org or the py launcher) and enginex-stemsd.exe (in
# PATH, next to this script, or $env:ENGINEX_STEMSD). Packages go into
# %USERPROFILE%\.enginex-stems\venv.
param(
    [ValidateSet('auto', 'hpss', 'demucs', 'htdemucs')][string]$Backend = $(if ($env:ENGINEX_STEMS_BACKEND) { $env:ENGINEX_STEMS_BACKEND } else { 'demucs' }),
    [switch]$AllowPublicFirewall
)
# Not 'Stop': Windows PowerShell 5.1 would turn a probe's stderr (2>$null) into an error. Failures throw.
$ErrorActionPreference = 'Continue'
$here = $PSScriptRoot
$home_ = if ($env:ENGINEX_STEMS_HOME) { $env:ENGINEX_STEMS_HOME } else { Join-Path $env:USERPROFILE '.enginex-stems' }
$venv = Join-Path $home_ 'venv'
$py = Join-Path $venv 'Scripts\python.exe'

$bin = if ($env:ENGINEX_STEMSD) { $env:ENGINEX_STEMSD } else { $env:SC5000_STEMSD }
if (-not $bin) { $bin = (Get-Command enginex-stemsd.exe -ErrorAction SilentlyContinue).Source }
if (-not $bin) { $bin = Join-Path $here 'enginex-stemsd.exe' }
if (-not (Test-Path $bin)) {
    throw "enginex-stemsd.exe not found: put the release binary next to this script or build it with 'go build ./cmd/enginex-stemsd'"
}
$bin = (Resolve-Path $bin).Path

function Invoke-Checked { & $args[0] $args[1..($args.Count - 1)]; if ($LASTEXITCODE) { throw "failed: $args" } }

if (-not (Test-Path $py)) {
    if (Get-Command py.exe -ErrorAction SilentlyContinue) { Invoke-Checked py.exe -3 -m venv $venv }
    else { Invoke-Checked python.exe -m venv $venv }
}
& $py -c 'import numpy, scipy' 2>$null
if ($LASTEXITCODE) { Invoke-Checked $py -m pip install -q numpy scipy }
if ($Backend -in 'auto', 'demucs', 'htdemucs') {
    $hasNvidia = [bool](Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue) -or (Test-Path (Join-Path $env:SystemRoot 'System32\nvidia-smi.exe'))
    & $py -c 'import demucs' 2>$null
    if ($LASTEXITCODE) {
        # pip's default Windows torch is CPU-only; an NVIDIA card wants the CUDA build.
        if ($hasNvidia) {
            & $py -m pip install -q torch torchaudio --index-url https://download.pytorch.org/whl/cu126 2>$null
            if ($LASTEXITCODE) {
                & $py -m pip install -q torch torchaudio --index-url https://download.pytorch.org/whl/cu124 2>$null
            }
        }
        if ($Backend -eq 'auto') {
            & $py -m pip install -q demucs # best effort: auto falls back to hpss
        } else {
            Invoke-Checked $py -m pip install -q demucs
        }
    }
}
$worker = Join-Path $here 'worker.py'
$resolvedBackend = (& $py $worker --backend $Backend --print-backend)
if ($resolvedBackend -eq 'htdemucs') {
    # Fetch the model weights now, not during a deck's first session.
    Invoke-Checked $py -c "from demucs.pretrained import get_model; get_model('htdemucs')"
}

# Ensure Windows Defender Firewall allows UDP 17891 / TCP 17890 on Domain,Private LocalSubnet (Public requires -AllowPublicFirewall).
try {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $isAdmin = $identity -and ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    $ruleName = 'EngineX Stems LAN Worker'
    if ($isAdmin) {
        Remove-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue | Out-Null
        if ($AllowPublicFirewall) {
            New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Program $bin -Profile Domain,Private,Public -RemoteAddress LocalSubnet -Enabled True -ErrorAction SilentlyContinue | Out-Null
        } else {
            New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Program $bin -Profile Domain,Private -RemoteAddress LocalSubnet -Enabled True -ErrorAction SilentlyContinue | Out-Null
        }
    } else {
        $appRules = Get-NetFirewallApplicationFilter -Program $bin -ErrorAction SilentlyContinue | Get-NetFirewallRule -ErrorAction SilentlyContinue | Where-Object { $_.Enabled -eq 'True' -and $_.Direction -eq 'Inbound' }
        if ($appRules | Where-Object { $_.Action -eq 'Block' }) {
            Write-Warning "EngineX OS: Windows Firewall has an active Inbound BLOCK rule for $bin. Decks on Wi-Fi/LAN cannot pair until you allow enginex-stemsd.exe on your Private home network (or run PowerShell as Administrator once; pass -AllowPublicFirewall only on a trusted home Wi-Fi marked Public)."
        } elseif (-not ($appRules | Where-Object { $_.Action -eq 'Allow' -and ($_.Profile -match 'Any|Private|Domain') })) {
            Write-Host "EngineX OS: Set your home Wi-Fi to Private in Windows Settings and allow enginex-stemsd.exe on Private networks when Windows Firewall prompts (pass -AllowPublicFirewall as Administrator only on a trusted home Wi-Fi marked Public)." -ForegroundColor Yellow
        }
    }
} catch {}

try {
    $ips = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
        Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } |
        Select-Object -ExpandProperty IPAddress
    if ($ips) {
        Write-Host ("EngineX OS: LAN Stems listener ({0}) on {1}:17890 (UDP discovery 17891)" -f $resolvedBackend, ($ips -join ', '))
    }
} catch {}

& $bin -listen '0.0.0.0:17890' '-token-file=' -python $py -worker $worker -backend $Backend -sessions 2
exit $LASTEXITCODE
