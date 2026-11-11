# Turn this Windows PC into the Stems renderer for EngineX decks on the same
# LAN. Decks find it by themselves (UDP 17891 / mDNS) and render on the deck
# again once it is switched off. Allow enginex-stemsd.exe on private networks
# when Windows Firewall asks.
#
#   powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1          # default: install demucs (htdemucs, NVIDIA CUDA or CPU)
#   powershell -ExecutionPolicy Bypass -File tools\stems\run-home-worker.ps1 hpss     # fast spectral split, numpy + scipy only
#
# Needs Python 3 (python.org or the py launcher) and enginex-stemsd.exe (in
# PATH, next to this script, or $env:ENGINEX_STEMSD). Packages go into
# %USERPROFILE%\.enginex-stems\venv.
param([ValidateSet('auto', 'hpss', 'demucs', 'htdemucs')][string]$Backend = $(if ($env:ENGINEX_STEMS_BACKEND) { $env:ENGINEX_STEMS_BACKEND } else { 'demucs' }))
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

function Invoke-Checked { & $args[0] $args[1..($args.Count - 1)]; if ($LASTEXITCODE) { throw "failed: $args" } }

if (-not (Test-Path $py)) {
    if (Get-Command py.exe -ErrorAction SilentlyContinue) { Invoke-Checked py.exe -3 -m venv $venv }
    else { Invoke-Checked python.exe -m venv $venv }
}
& $py -c 'import numpy, scipy' 2>$null
if ($LASTEXITCODE) { Invoke-Checked $py -m pip install -q numpy scipy }
if ($Backend -in 'demucs', 'htdemucs') {
    & $py -c 'import demucs' 2>$null
    if ($LASTEXITCODE) {
        # pip's default Windows torch is CPU-only; an NVIDIA card wants the CUDA build.
        if (Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue) {
            Invoke-Checked $py -m pip install -q torch torchaudio --index-url https://download.pytorch.org/whl/cu124
        }
        Invoke-Checked $py -m pip install -q demucs
    }
}
$worker = Join-Path $here 'worker.py'
if ((& $py $worker --backend $Backend --print-backend) -eq 'htdemucs') {
    # Fetch the model weights now, not during a deck's first session.
    Invoke-Checked $py -c "from demucs.pretrained import get_model; get_model('htdemucs')"
}
& $bin -listen 0.0.0.0:17890 -token-file= -python $py -worker $worker -backend $Backend -sessions 2
exit $LASTEXITCODE
