// #336 E4 Rule 0.1 real-Engine + real-enginex-suite restart-recovery probe.
// Drives real Settings SwitchEntry controls against a live compiled ARMv7 enginex-suite
// across both a persisted-ui.rev restart and an ephemeral (ui.rev removed, rev=1) restart
// while Engine stays open throughout.
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
    property int waits: 0
    property string epoch1: ""
    property int rev1: 0
    property int revAfter1: 0
    property string epoch2: ""
    property int rev2: 0
    property int revAfter2: 0
    property string epoch3: ""
    property int rev3: 0
    property var tabs: []
    property var swMaster: null
    property var swAutoGain: null
    property var swSoundCloud: null
    property var swCue: null

    function quit() { Planck.getProperty("/Client/Quit").translator.state = true }
    function check(ok, name, detail) {
        if (ok) {
            passes++
            console.warn("E4REAL PASS|" + board + "|" + name + "|" + (detail || ""))
        } else {
            fails++
            console.warn("E4REAL FAIL|" + board + "|" + name + "|" + (detail || ""))
        }
    }
    function walk(o, fn, seen) {
        if (!o || seen.has(o)) return
        seen.add(o)
        fn(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], fn, seen)
        if (o.content && o.content.length !== undefined) for (let i = 0; i < o.content.length; i++) walk(o.content[i], fn, seen)
    }
    function finish() {
        console.warn("E4REAL SUMMARY|" + board + "|pass=" + passes + "|fail=" + fails + "|")
        console.warn("E4REAL DONE|" + (fails === 0 ? "PASS" : "FAIL") + "|" + board + "|")
        quit()
    }
    function runStep() {
        try {
            if (step === 0) {
                if (!root.scUiLoaded || !root.scUiEpoch || root.scUiRev < 1) {
                    if (++waits > 50) { check(false, "initial_load_timeout", "loaded=" + root.scUiLoaded + " epoch=" + root.scUiEpoch + " rev=" + root.scUiRev); finish(); return }
                    root.scCall("")
                    tick.interval = 250
                    tick.start()
                    return
                }
                waits = 0
                epoch1 = root.scUiEpoch
                rev1 = root.scUiRev
                check(epoch1.length > 0 && rev1 >= 1, "proc1_loaded", "epoch=" + epoch1 + " rev=" + rev1)
                const pages = [
                    ["Services", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/ServicesTabContent.qml"],
                    ["Playback", "qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/PlaybackTabContent.qml"],
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
                for (const t of tabs) {
                    walk(t.obj, function(o) {
                        if (o.objectName === "Entry_Switch" && o.parent && o.parent.visible) {
                            const txt = String(o.parent.text || "")
                            if (!swMaster && txt === "Master Soft Clipper") swMaster = o
                            if (!swAutoGain && txt === "Auto-Gain") swAutoGain = o
                            if (!swSoundCloud && txt === "SoundCloud") swSoundCloud = o
                            if (!swCue && txt === "Cue Countdown") swCue = o
                        }
                    }, new Set())
                }
                check(!!swMaster && !!swAutoGain && !!swSoundCloud && !!swCue, "controls_found")
                if (!swMaster || !swAutoGain || !swSoundCloud || !swCue) { finish(); return }
                // Pre-restart single-tap toggles: Auto-Gain ON, Master Soft Clipper ON, SoundCloud OFF.
                swAutoGain.parent.clicked()
                swMaster.parent.clicked()
                swSoundCloud.parent.clicked()
                check(root.scAutoGain === true && swAutoGain.checked === true && root.scMasterLimiter === true && swMaster.checked === true && root.scSources.indexOf("soundcloud") < 0, "proc1_optimistic")
                step = 2
                tick.interval = 250
                tick.start()
            } else if (step === 2) {
                if (root.scUiPending > 0) {
                    if (++waits > 30) { check(false, "proc1_drain_timeout", "pending=" + root.scUiPending); finish(); return }
                    tick.interval = 200
                    tick.start()
                    return
                }
                waits = 0
                revAfter1 = root.scUiRev
                check(root.scUiEpoch === epoch1 && revAfter1 > rev1 && root.scAutoGain === true && root.scMasterLimiter === true && root.scSources.indexOf("soundcloud") < 0,
                    "proc1_confirmed", "epoch=" + root.scUiEpoch + " rev=" + revAfter1)
                console.warn("E4REAL TRIGGER_RESTART_1|" + board + "|epoch=" + epoch1 + "|rev=" + revAfter1 + "|")
                step = 3
                tick.interval = 350
                tick.start()
            } else if (step === 3) {
                // Wait for real enginex-suite restart #1 (persisted ui.rev: new epoch2, rev2 > revAfter1).
                if (!root.scUiEpoch || root.scUiEpoch === epoch1) {
                    if (++waits > 45) { check(false, "restart1_recovery_timeout", "epoch=" + root.scUiEpoch + " rev=" + root.scUiRev); finish(); return }
                    root.scCall("")
                    tick.interval = 300
                    tick.start()
                    return
                }
                waits = 0
                epoch2 = root.scUiEpoch
                rev2 = root.scUiRev
                check(epoch2 !== epoch1 && rev2 > revAfter1 && !!(root.scRetiredEpochs && root.scRetiredEpochs[epoch1]),
                    "restart1_epoch_and_persisted_rev", "epoch1=" + epoch1 + " epoch2=" + epoch2 + " rev=" + revAfter1 + "->" + rev2)
                check(root.scAutoGain === true && swAutoGain.checked === true && root.scMasterLimiter === true && swMaster.checked === true && root.scSources.indexOf("soundcloud") < 0 && swSoundCloud.checked === false,
                    "restart1_state_preserved")
                // Single-tap toggle after restart #1: Cue Countdown OFF.
                swCue.parent.clicked()
                check(root.scCueCountdown === false && swCue.checked === false, "restart1_cue_optimistic")
                step = 4
                tick.interval = 250
                tick.start()
            } else if (step === 4) {
                if (root.scUiPending > 0) {
                    if (++waits > 30) { check(false, "proc2_drain_timeout", "pending=" + root.scUiPending); finish(); return }
                    tick.interval = 200
                    tick.start()
                    return
                }
                waits = 0
                revAfter2 = root.scUiRev
                check(root.scUiEpoch === epoch2 && revAfter2 > rev2 && root.scCueCountdown === false && swCue.checked === false,
                    "restart1_single_tap_confirmed", "epoch=" + root.scUiEpoch + " rev=" + revAfter2)
                console.warn("E4REAL TRIGGER_RESTART_2|" + board + "|epoch=" + epoch2 + "|rev=" + revAfter2 + "|")
                step = 5
                tick.interval = 350
                tick.start()
            } else if (step === 5) {
                // Wait for real enginex-suite restart #2 (ui.rev removed -> ephemeral rev=1 < revAfter2, new epoch3).
                if (!root.scUiEpoch || root.scUiEpoch === epoch2) {
                    if (++waits > 45) { check(false, "restart2_recovery_timeout", "epoch=" + root.scUiEpoch + " rev=" + root.scUiRev); finish(); return }
                    root.scCall("")
                    tick.interval = 300
                    tick.start()
                    return
                }
                waits = 0
                epoch3 = root.scUiEpoch
                rev3 = root.scUiRev
                check(epoch3 !== epoch2 && epoch3 !== epoch1 && rev3 === 1 && rev3 < revAfter2 && !!(root.scRetiredEpochs && root.scRetiredEpochs[epoch2]),
                    "restart2_ephemeral_low_rev_recovered", "epoch2=" + epoch2 + " epoch3=" + epoch3 + " rev=" + revAfter2 + "->" + rev3)
                check(root.scAutoGain === true && root.scMasterLimiter === true && root.scCueCountdown === false && root.scSources.indexOf("soundcloud") < 0,
                    "restart2_state_preserved")
                // Single-tap toggles after ephemeral restart: Auto-Gain OFF + SoundCloud ON.
                swAutoGain.parent.clicked()
                swSoundCloud.parent.clicked()
                check(root.scAutoGain === false && swAutoGain.checked === false && root.scSources.indexOf("soundcloud") >= 0 && swSoundCloud.checked === true,
                    "restart2_single_tap_optimistic")
                step = 6
                tick.interval = 250
                tick.start()
            } else if (step === 6) {
                if (root.scUiPending > 0) {
                    if (++waits > 30) { check(false, "proc3_drain_timeout", "pending=" + root.scUiPending); finish(); return }
                    tick.interval = 200
                    tick.start()
                    return
                }
                check(root.scUiEpoch === epoch3 && root.scUiRev > rev3 && root.scAutoGain === false && swAutoGain.checked === false && root.scMasterLimiter === true && root.scCueCountdown === false && root.scSources.indexOf("soundcloud") >= 0 && swSoundCloud.checked === true,
                    "restart2_single_tap_confirmed", "epoch=" + root.scUiEpoch + " rev=" + root.scUiRev)
                finish()
            }
        } catch (e) {
            fails++
            console.warn("E4REAL EXC|" + board + "|" + e + "|")
            finish()
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
