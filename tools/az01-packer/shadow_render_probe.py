#!/usr/bin/env python3
"""Generate a native Engine QML display probe. Sandbox only, never ship it."""
import argparse
import importlib.util
import json
from pathlib import Path
import re


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()
    spec = importlib.util.spec_from_file_location("packs", Path(__file__).parents[1] / "ui-mod/build_packs.py")
    bp = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bp)
    names = ("scDeckLocked", "scShadowVisible", "scShadowDeckFor", "scShadowData", "scShadowPosition",
             "scShadowBeatIndex", "scShadowBeatAt", "scShadowGrid", "scShadowNextCue", "scShadowCuesFor", "scShadowAsset")
    functions = []
    for name in names:
        m = re.search(r"(?m)^\tfunction " + name + r"\([^\n]*\) \{", bp.SERVICES_MAIN)
        end = bp.SERVICES_MAIN.index("\n", m.start())
        if not bp.SERVICES_MAIN[m.start():end].rstrip().endswith("}"):
            end = bp.SERVICES_MAIN.index("\n\t}", m.end()) + 3
        functions.append(bp.SERVICES_MAIN[m.start():end])
    meta = dict(key="fixture", title="Source waveform", artist="Display-only native QML probe", bpm=120,
                keyName="8A", durationMs=10000, sampleRate=44100,
                beats=[dict(ms=i*500, number=i) for i in range(21)],
                cues=[dict(i=1, ms=6000, n="DROP", c="#ffb400")],
                loops=[dict(startMs=7000, endMs=8000, n="LOOP", c="#4a78ff")],
                detail=True, waveformWidth=4096, tileWidth=2048,
                waveformTiles=["http://127.0.0.1:8080/tile.png"]*2,
                waveformUrl="http://127.0.0.1:8080/tile.png", overviewUrl="http://127.0.0.1:8080/tile.png")
    samples = {}
    for d in (2, 4):
        m = dict(meta)
        if d == 4:
            m.update(detail=False, waveformTiles=[])
        samples[d] = dict(key="fixture", title=m["title"], artist=m["artist"], meta=m,
                          positionMs=2000, at=0, playing=True, pitch=0, master=d == 2, sync=d == 4)
    components = []
    for kind in ("waveform", "overview", "track"):
        layer = bp.SHADOW_LAYER.replace("@KIND@", kind).replace("@OWNER@", "owner")
        components.append("Component { id: " + kind + "Component; Item { id: owner; "
                          "property int deckIndex: parent.deckIndex; width:parent.width; height:parent.height; " + layer + "} }")
    qml = """import QtQuick 2.15
import QtQuick.Window 2.15
import QtQuick.Layouts 1.15
Item {
 id: root
 width: 1200; height: 700; z: 30000
 property var transformed: root
 property string scFourDeckRole: "13"
 property bool scPlayer: true
 property bool scShadowDisplayOnly: true
 property bool scCueCountdown: true
 property int scShadowExpiryMs: 3000
 property int scShadowFrame: 0
 property int scCueTick: 0
 property var scZoomProp: null
 property var scShadowAt: ({})
 property var scShadowGridCache: ({})
 property var scShadowLiveCueCache: ({})
 property var scShadowEmptyArray: []
 property var scShadowDisplay: @SAMPLES@
 Rectangle { anchors.fill:parent; color:"#000000"; z:-1 }
 @FUNCTIONS@
 @COMPONENTS@
 Loader { x:0; y:0; width:1200; height:155; property int deckIndex:2; sourceComponent:waveformComponent }
 Loader { x:0; y:160; width:1200; height:55; property int deckIndex:2; sourceComponent:overviewComponent }
 Loader { x:0; y:220; width:1200; height:85; property int deckIndex:2; sourceComponent:trackComponent }
 Loader { id: testWave4; x:0; y:335; width:1200; height:155; property int deckIndex:4; sourceComponent:waveformComponent }
 Loader { x:0; y:495; width:1200; height:55; property int deckIndex:4; sourceComponent:overviewComponent }
 Loader { x:0; y:555; width:1200; height:85; property int deckIndex:4; sourceComponent:trackComponent }
 Component.onCompleted: {
   for (let d of [2,4]) scShadowDisplay[d].at=Date.now()
   console.warn("SHADOWPROBE loaded")
 }
 Timer { interval:40; running:true; repeat:true; onTriggered: {
   root.scShadowFrame++; root.scCueTick++
   // Keep the fixture moving without synthetic Engine decks or a network clock.
   for (let d of [2,4]) if(Date.now()-root.scShadowDisplay[d].at>1000) {
     root.scShadowDisplay[d].positionMs=2000; root.scShadowDisplay[d].at=Date.now()
   }
 } }
 Timer { interval:1800; running:true; onTriggered: testWave4.visible=false }
 Timer { interval:2200; running:true; onTriggered: {
   let found=false, hidden=false
   for(let i=0;i<root.children.length;i++) {
     const c=root.children[i]
     if(c.objectName === "EngineX_Shadow_waveform" && c.deck === 4) {found=true; hidden=!c.visible}
   }
   console.warn("SHADOWPROBE ancestor-hidden|"+(found && hidden))
   testWave4.visible=true
 } }
 Timer { interval:6000; running:true; onTriggered: root.grabToImage(function(result) {
   console.warn("SHADOWPROBE screenshot|"+result.saveToFile("/tmp/shadow-render.png"))
   Qt.quit()
 }) }
}
"""
    qml = qml.replace("@SAMPLES@", json.dumps(samples)).replace("@FUNCTIONS@", "\n".join(functions)).replace("@COMPONENTS@", "\n".join(components))
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(qml)


if __name__ == "__main__":
    main()
