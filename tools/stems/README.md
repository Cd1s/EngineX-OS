# EngineX Stems Computer Service / 电脑高音质分轨服务 / 電腦高音質分軌服務

## English

Run this service on a Mac or PC on the same Wi-Fi / LAN as your deck for high-quality neural Stems (`htdemucs`, GPU/MPS/CPU). Your deck discovers it automatically (UDP 17891 / mDNS) with no IP or password setup, and falls back to on-deck separation when the computer is turned off.

1. Install **Python 3** (`python3` on macOS/Linux, or from <https://www.python.org> on Windows).
2. Open a terminal in the extracted installer folder and run:
   - **macOS / Linux**: `./stems/run-home-worker.sh`
   - **Windows (PowerShell)**: `powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1`
   - *(Optional lightweight spectral mode without PyTorch)*: pass `hpss` at the end of the command.
   - *(Python-free single binary: Spleeter instant pass + background HTDemucs HQ cache)*: `./stems/enginex-stemsd -native -backend tiered -auto-download-models` (`.\stems\enginex-stemsd.exe -native -backend tiered -auto-download-models` on Windows).

---

## 简体中文

在与碟机同一 Wi-Fi / 局域网的电脑上运行本服务，即可使用高音质神经网络分轨（默认自动安装并启用 `htdemucs`，支持显卡 / Apple MPS / CPU）。碟机会通过局域网自动发现电脑（UDP 17891 / mDNS），无需输入 IP 或密码；关掉电脑后自动切回碟机本机基础分轨。

1. 在电脑上安装 **Python 3**（macOS / Linux 自带或安装 `python3`，Windows 可从 <https://www.python.org> 下载）。
2. 在解压后的安装器目录打开终端，运行对应命令：
   - **macOS / Linux**：`./stems/run-home-worker.sh`
   - **Windows（PowerShell）**：`powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1`
   - *（可选轻量频谱模式，免装 PyTorch）*：在命令末尾加 `hpss`。
   - *（免 Python 单文件模式：Spleeter 秒级分轨 + 后台 HTDemucs 高音质缓存）*：`./stems/enginex-stemsd -native -backend tiered -auto-download-models`（Windows 为 `.\stems\enginex-stemsd.exe -native -backend tiered -auto-download-models`）。

---

## 繁體中文

在與播放機同一 Wi-Fi / 區域網路的電腦上執行本服務，即可使用高音質神經網路分軌（預設自動安裝並啟用 `htdemucs`，支援顯示卡 / Apple MPS / CPU）。播放機會透過區域網路自動探索電腦（UDP 17891 / mDNS），無需輸入 IP 或密碼；關閉電腦後自動切回播放機本機基礎分軌。

1. 在電腦上安裝 **Python 3**（macOS / Linux 內建或安裝 `python3`，Windows 可自 <https://www.python.org> 下載）。
2. 在解壓後的安裝器目錄開啟終端機，執行對應指令：
   - **macOS / Linux**：`./stems/run-home-worker.sh`
   - **Windows（PowerShell）**：`powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1`
   - *（可選輕量頻譜模式，免裝 PyTorch）*：在指令末尾加上 `hpss`。
   - *（免 Python 單執行檔模式：Spleeter 秒級分軌 + 背景 HTDemucs 高音質快取）*：`./stems/enginex-stemsd -native -backend tiered -auto-download-models`（Windows 為 `.\stems\enginex-stemsd.exe -native -backend tiered -auto-download-models`）。
