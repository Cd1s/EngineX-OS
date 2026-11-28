// #336 E4 sandbox probe: exercises real Settings SwitchEntry / ListEntry / STREAM_SWITCHES /
// Remote access controls against a local loopback server with deterministic poll/POST races.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    property int step: 0
    property int fails: 0
    property int passes: 0
    property var tabs: []
    property var swMaster: null
    property var swAutoGain: null
    property var swSpotify: null
    property var swSoundCloud: null
    property var swDeezer: null
    property var swCue: null
    property var swRemote: null
    property var leLed: null

    function quit() { Planck.getProperty("/Client/Quit").translator.state = true }
    function check(ok, name) {
        if (ok) {
            passes++
            console.warn("E4SBX PASS|" + board + "|" + name + "|")
        } else {
            fails++
            console.warn("E4SBX FAIL|" + board + "|" + name + "|")
        }
    }
    function walk(o, fn, seen) {
        if (!o || seen.has(o)) return
        seen.add(o)
        fn(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], fn, seen)
        if (o.content && o.content.length !== undefined) for (let i = 0; i < o.content.length; i++) walk(o.content[i], fn, seen)
    }
    function runStep() {
        try {
            if (step === 0) {
                if (!root.scUiLoaded) { tick.interval = 200; tick.start(); return }
                const pages = [
                    ["Device", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/DeviceTabContent.qml"],
                    ["Services", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/ServicesTabContent.qml"],
                    ["Playback", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/PlaybackTabContent.qml"],
                    ["Library", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/LibraryTabContent.qml"],
                    ["Layout", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/LayoutManagerTab.qml"]
                ]
                for (const [name, url] of pages) {
                    const c = Qt.createComponent(url)
                    check(c.status === Component.Ready, "load_" + name)
                    if (c.status !== Component.Ready) continue
                    const obj = c.createObject(probe.Window.window.contentItem, {width: 1104, height: 640, z: 1000})
                    tabs.push({name: name, obj: obj})
                }
                step = 1
                tick.interval = 250
                tick.start()
            } else if (step === 1) {
                // 1. Every SwitchEntry control must keep checked and position synced with checkedExternally.
                let mismatches = 0
                let posMismatches = 0
                for (const t of tabs) {
                    walk(t.obj, function(o) {
                        if (o.objectName === "Entry_Switch" && o.parent) {
                            const txt = String(o.parent.text || "")
                            if (o.checked !== o.checkedExternally) mismatches++
                            const wantPos = o.checkedExternally ? 1.0 : 0.0
                            if (Math.abs(o.position - wantPos) > 0.01) posMismatches++
                            if (o.parent.visible) {
                                if (!swMaster && txt === "Master Soft Clipper") swMaster = o
                                if (!swAutoGain && txt === "Auto-Gain") swAutoGain = o
                                if (!swSpotify && txt === "Spotify") swSpotify = o
                                if (!swSoundCloud && txt === "SoundCloud") swSoundCloud = o
                                if (!swDeezer && txt === "Deezer") swDeezer = o
                                if (!swCue && txt === "Cue Countdown") swCue = o
                                if (!swRemote && o.parent.objectName === "EngineX_RemoteAccess") swRemote = o
                            }
                        }
                        if (!leLed && o.text === "Pad LED Brightness" && o.values) leLed = o
                    }, new Set())
                }
                check(!!swMaster && !!swAutoGain && !!swSpotify && !!swSoundCloud && !!swDeezer && !!swCue && !!swRemote && !!leLed, "controls_found")
                check(mismatches === 0, "switch_checked_matches_external_" + mismatches)
                check(posMismatches === 0, "switch_position_matches_external_" + posMismatches)

                // 2. STREAM_SWITCHES (Spotify) must stay aligned when toggled before scUiLoaded.
                const savedLoaded = root.scUiLoaded
                root.scUiLoaded = false
                swSpotify.checked = !swSpotify.checked
                swSpotify.toggled()
                root.scUiLoaded = savedLoaded
                check(swSpotify.checked === true && swSpotify.checkedExternally === true, "stream_switch_not_inverted_when_unloaded")

                // 3a. Enable Auto-Gain first so server digest moves from d0 -> d1.
                swAutoGain.parent.clicked()
                check(root.scAutoGain === true && swAutoGain.checkedExternally === true && swAutoGain.checked === true, "autogain_optimistic")
                step = 2
                tick.interval = 250
                tick.start()
            } else if (step === 2) {
                // 3b. Start a 3s poll (server snapshots masterLimiter=false immediately, then sleeps 400ms).
                root.scCall("")
                step = 3
                tick.interval = 100
                tick.start()
            } else if (step === 3) {
                // 3c. 100ms later (poll has already snapshotted masterLimiter=false), user clicks Master Soft Clipper.
                swMaster.parent.clicked()
                check(root.scMasterLimiter === true && swMaster.checkedExternally === true && swMaster.checked === true, "master_limiter_optimistic")
                step = 4
                tick.interval = 500
                tick.start()
            } else if (step === 4) {
                // Stale poll has now returned with masterLimiter=false; setting must NOT have reverted!
                check(root.scMasterLimiter === true && swMaster.checkedExternally === true && swMaster.checked === true, "master_limiter_survives_stale_poll")

                // 4. Rapidly disable two streaming services while discovered rekordbox is visible (SoundCloud POST delayed 200ms).
                swSoundCloud.parent.clicked()
                swDeezer.parent.clicked()
                check(root.scSources.indexOf("soundcloud") < 0 && root.scSources.indexOf("deezer") < 0 && root.scSources.indexOf("rekordbox") >= 0, "rapid_sources_optimistic")
                step = 5
                tick.interval = 500
                tick.start()
            } else if (step === 5) {
                check(root.scSources.indexOf("soundcloud") < 0 && root.scSources.indexOf("deezer") < 0 && root.scSources.indexOf("rekordbox") >= 0, "rapid_sources_both_persisted")

                // 5. Rapid ListEntry picks: 80% (delayed 250ms on server) then 60% (fast).
                leLed.selected(1)
                leLed.selected(2)
                check(root.scLedBrightness === 60, "rapid_led_optimistic")
                step = 6
                tick.interval = 550
                tick.start()
            } else if (step === 6) {
                check(root.scLedBrightness === 60, "rapid_led_keeps_latest_60")

                // 6. Remote access toggle with in-flight UI poll (poll snapshots tunnel=false, sleeps 200ms; POST sleeps 250ms).
                root.scCall("")
                step = 7
                tick.interval = 80
                tick.start()
            } else if (step === 7) {
                swRemote.parent.clicked()
                check(root.scRemoteAccess === true && swRemote.checkedExternally === true && swRemote.checked === true, "remote_access_optimistic")
                step = 8
                tick.interval = 450
                tick.start()
            } else if (step === 8) {
                check(root.scRemoteAccess === true && swRemote.checkedExternally === true && swRemote.checked === true, "remote_access_survives_stale_poll")
                console.warn("E4SBX SUMMARY|" + board + "|pass=" + passes + "|fail=" + fails + "|")
                console.warn("E4SBX DONE|" + (fails === 0 ? "PASS" : "FAIL") + "|" + board + "|")
                quit()
            }
        } catch (e) {
            fails++
            console.warn("E4SBX EXC|" + board + "|" + e + "|")
            console.warn("E4SBX DONE|FAIL|" + board + "|")
            quit()
        }
    }
    Timer {
        id: tick
        interval: 250
        repeat: false
        running: true
        onTriggered: probe.runStep()
    }
}
