"""Run on a fresh, isolated JP07 engine-mode twin (up --local --mode engine)."""
import os
import time

from twin import WORK, ev, evj


def wait(side, expr):
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        if evj(side, "JSON.stringify(!!(" + expr + "))"):
            return
    raise AssertionError(expr)


if __name__ == "__main__":
    if not os.environ.get("SHADOW_TWIN_WORK"):
        raise SystemExit("set SHADOW_TWIN_WORK to the isolated twin")
    browse = "root.rtTr('/GUI/ShowBrowseView')"
    slot = "root.scShadowAt[2]"
    ev("A", "root.scCall('pairedView=1'); 'paired'")
    wait("A", "root.scPairedOn")
    ev("A", f"{browse}.state=true; 'browsing'")
    ev("B", "root.rtTr('/Client/Librarian/AddToDatabaseAndLoadToDeck2').url='file:///data/music/track-b.wav'; 'issued'")
    wait("A", f"{slot} && {slot}.pendingLoad && !{slot}.loads && {browse}.state")
    # Reopen Browse after the deferred URL has actually been issued. Observe
    # transitions, not just the final state: a close/reopen would lose focus too.
    ev("A", f"""
        var url=root.rtTr('/Client/Librarian/AddToDatabaseAndLoadToDeck2'), br={browse};
        var again=function() {{ url.urlChanged.disconnect(again); Qt.callLater(function() {{
            br.state=true; console.warn('BROWSE_PROBE_OPEN');
            br.stateChanged.connect(function() {{ if(!br.state) console.warn('BROWSE_PROBE_CLOSED') }});
        }}) }};
        url.urlChanged.connect(again); br.state=false; 'released'
    """)
    wait("A", f"{slot} && {slot}.ready && root.rtTr('/Engine/Deck2/Track/FileName').string==='track-b.wav'")
    log = (WORK / "A/logs/engine.log").read_text(errors="replace")
    assert "BROWSE_PROBE_OPEN" in log and "BROWSE_PROBE_CLOSED" not in log
    assert evj("A", f"JSON.stringify({browse}.state)")
    # Same physical file, submitted by the user to their own deck.
    ev("A", f"root.rtTr('/Client/Librarian/AddToDatabaseAndLoadToDeck1').url='file://'+encodeURI({slot}.path); 'manual'")
    wait("A", f"!{browse}.state && root.rtTr('/Engine/Deck1/Track/FileName').string==='track-b.wav'")
    print("PASS: latest shadow loaded, Browse never closed in flight; same-file manual load closed Browse")
