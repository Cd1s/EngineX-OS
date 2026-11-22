# tools/mapping

`inventory.py` reads the stock controller assignment files and qrc modules of an unpacked Engine 5.3.0
firmware and writes one JSON file per board, one row per control, with a stable id
`<module>:<note>` (`PadModeSelect:30`, `Sync:20`; CCs are `<module>:cc<n>`).

The four player boards (JP07, JP08, JP13, JP14) are committed in `pkg/mapping/data/`; the console reads them
(`go:embed`). Regenerate them from the firmware lab:

```sh
python3 -I tools/mapping/inventory.py \
  --assignments <firmware-lab>/5.3.0/tree/usr/Engine/AssignmentFiles/PresetAssignmentFiles \
  --qrc <firmware-lab>/5.3.0/qrc --boards JP07 JP08 JP13 JP14
python3 -m unittest discover -s tools/mapping
```

The stock-function texts are the extractor's reading of the module defaults; they are shown in the console
as information. Which gestures a DJ may take is decided by `pkg/mapping/catalogue.json`, not by these texts.
