// #336 S23 Rule 0.1 real-Engine + real-enginex-suite search-filter persistence probe.
// Loaded at Skins/Common/Items/QmlWarnings.qml inside real 5.3.0 EmbeddedMain.qml.
// Modes:
//   stock_boot1: unmodified stock 5.3.0 Search/SearchMenuDialog sets 5 fields + SearchAll=true.
//   stock_boot2: unmodified stock 5.3.0 restart observes fields dropped back to song,artist.
//   s23_boot1:   S23+E4 pack + real ARMv7 enginex-suite sets BPM/Key/Comment via real SearchMenuDialog + SearchAll=true.
//   s23_boot2:   restart Engine + enginex-suite, verify 5 fields restored in librarianController & Search dialog,
//                verify SearchAll preserved independently, then trigger ResetAll and confirm defaults restored.
//   s23_boot3:   restart Engine + enginex-suite after ResetAll, verify clean stock default song,artist.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    readonly property string mode: "@MODE@"
    readonly property var canonicalOrder: ["song", "artist", "album", "length", "key", "comment", "bpm", "genre", "label", "year", "dateImported", "filename"]
    property int step: 0
    property int fails: 0
    property int passes: 0
    property int waits: 0
    property int revBeforeSave: 0
    property int revBeforeReset: 0
    property var hostWrap: null
    property var searchObj: null
    property var refineBtn: null

    function quit() {
        const q = Planck.getProperty("/Client/Quit")
        if (q && q.translator) q.translator.state = true
    }
    function check(ok, name, detail) {
        if (ok) {
            passes++
            console.warn("S23REAL PASS|" + board + "|" + mode + "|" + name + "|" + (detail || ""))
        } else {
            fails++
            console.warn("S23REAL FAIL|" + board + "|" + mode + "|" + name + "|" + (detail || ""))
        }
    }
    function rolesCanonical(list) {
        const seen = {}
        if (list) {
            for (let i = 0; i < list.length; i++) seen[String(list[i])] = true
        }
        const out = []
        for (let i = 0; i < canonicalOrder.length; i++) {
            if (seen[canonicalOrder[i]]) out.push(canonicalOrder[i])
        }
        return out.join(",")
    }
    function objRolesCanonical(obj) {
        if (!obj) return ""
        return rolesCanonical(Object.keys(obj))
    }
    function engineRoles() {
        return rolesCanonical(Planck.librarianController.searchConstraintRolesList())
    }
    function getSearchAll() {
        const p = Planck.getProperty("/Client/Librarian/SearchAll")
        return !!(p && p.translator && p.translator.state)
    }
    function setSearchAll(v) {
        const p = Planck.getProperty("/Client/Librarian/SearchAll")
        if (p && p.translator) p.translator.state = !!v
    }
    function walk(o, fn, seen) {
        if (!o || seen.has(o)) return
        seen.add(o)
        fn(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], fn, seen)
        if (o.content && o.content.length !== undefined) for (let i = 0; i < o.content.length; i++) walk(o.content[i], fn, seen)
    }
    function findSearch() {
        let foundSearch = null
        let foundBtn = null
        const content = probe.Window && probe.Window.window ? probe.Window.window.contentItem : null
        if (content) {
            walk(content, function(o) {
                if (!foundSearch && o.objectName === "Search" && o.searchConstraints !== undefined) foundSearch = o
                if (!foundBtn && o.objectName === "SearchCriteriaMenu_Button") foundBtn = o
            }, new Set())
        }
        if (foundSearch && foundBtn) {
            searchObj = foundSearch
            refineBtn = foundBtn
            return true
        }
        if (!hostWrap && content) {
            hostWrap = Qt.createQmlObject(
                'import QtQuick 2.12\n' +
                'import "qrc:/Skins/Embedded/Views/BrowseView/Components"\n' +
                'Item { id: wrap; width: 640; height: 80; z: 1000; property var statusBar: ({height: 64}); Search { id: s; objectName: "Search"; anchors.fill: parent; visible: true; hasSearchRefineMenu: true } property alias sItem: s }',
                content,
                "s23SearchHost"
            )
            if (hostWrap && hostWrap.sItem) {
                searchObj = hostWrap.sItem
                walk(searchObj, function(o) {
                    if (!refineBtn && o.objectName === "SearchCriteriaMenu_Button") refineBtn = o
                }, new Set())
                return !!(searchObj && refineBtn)
            }
        }
        return !!(searchObj && refineBtn)
    }
    function finish() {
        console.warn("S23REAL SUMMARY|" + board + "|" + mode + "|pass=" + passes + "|fail=" + fails + "|roles=" + engineRoles() + "|searchAll=" + getSearchAll() + "|")
        console.warn("S23REAL DONE|" + (fails === 0 ? "PASS" : "FAIL") + "|" + board + "|" + mode + "|")
        quit()
    }
    function runStep() {
        try {
            if (mode === "stock_boot1") {
                if (step === 0) {
                    const pBrowse = Planck.getProperty("/GUI/ShowBrowseView")
                    if (pBrowse && pBrowse.translator) pBrowse.translator.state = true
                    const pBox = Planck.getProperty("/Client/Librarian/SearchBoxOpen")
                    if (pBox && pBox.translator) pBox.translator.state = true
                    step = 1
                    tick.interval = 350
                    tick.start()
                    return
                }
                if (step === 1) {
                    if (!findSearch()) {
                        if (++waits > 20) { check(false, "search_not_found"); finish(); return }
                        tick.interval = 200
                        tick.start()
                        return
                    }
                    check(engineRoles() === "song,artist", "stock_initial_default", "roles=" + engineRoles())
                    setSearchAll(false)
                    refineBtn.clicked()
                    step = 2
                    tick.interval = 250
                    tick.start()
                    return
                }
                if (step === 2) {
                    check(!!searchObj.dialog, "stock_dialog_opened")
                    if (!searchObj.dialog) { finish(); return }
                    searchObj.dialog.setConstraint("bpm", true)
                    searchObj.dialog.setConstraint("key", true)
                    searchObj.dialog.setConstraint("comment", true)
                    check(engineRoles() === "song,artist,key,comment,bpm", "stock_boot1_custom_active", "roles=" + engineRoles())
                    setSearchAll(true)
                    check(getSearchAll() === true, "stock_boot1_searchAll_on")
                    step = 3
                    tick.interval = 400
                    tick.start()
                    return
                }
                finish()
            } else if (mode === "stock_boot2") {
                if (step === 0) {
                    step = 1
                    tick.interval = 600
                    tick.start()
                    return
                }
                const roles = engineRoles()
                const sa = getSearchAll()
                check(roles === "song,artist", "stock_boot2_reverted_to_default", "roles=" + roles)
                if (board === "JP11") {
                    check(sa === true, "stock_boot2_searchAll_kept", "searchAll=" + sa)
                } else {
                    console.warn("S23REAL INFO|" + board + "|" + mode + "|searchAll=" + sa + "|")
                }
                finish()
            } else if (mode === "s23_boot1") {
                if (step === 0) {
                    if (!root.scUiLoaded || !root.scUiEpoch || root.scUiRev < 1 || !root.scSearchFieldsRestored) {
                        if (++waits > 50) { check(false, "ui_load_timeout", "loaded=" + root.scUiLoaded + " rev=" + root.scUiRev); finish(); return }
                        root.scCall("")
                        tick.interval = 250
                        tick.start()
                        return
                    }
                    waits = 0
                    revBeforeSave = root.scUiRev
                    check(engineRoles() === "song,artist", "s23_boot1_default", "roles=" + engineRoles() + " rev=" + revBeforeSave + " epoch=" + root.scUiEpoch)
                    const pBrowse = Planck.getProperty("/GUI/ShowBrowseView")
                    if (pBrowse && pBrowse.translator) pBrowse.translator.state = true
                    const pBox = Planck.getProperty("/Client/Librarian/SearchBoxOpen")
                    if (pBox && pBox.translator) pBox.translator.state = true
                    step = 1
                    tick.interval = 350
                    tick.start()
                    return
                }
                if (step === 1) {
                    if (!findSearch()) {
                        if (++waits > 20) { check(false, "search_not_found"); finish(); return }
                        tick.interval = 200
                        tick.start()
                        return
                    }
                    setSearchAll(false)
                    check(getSearchAll() === false, "s23_boot1_searchAll_off")
                    refineBtn.clicked()
                    step = 2
                    tick.interval = 250
                    tick.start()
                    return
                }
                if (step === 2) {
                    check(!!searchObj.dialog, "s23_dialog_opened")
                    if (!searchObj.dialog) { finish(); return }
                    searchObj.dialog.setConstraint("bpm", true)
                    searchObj.dialog.setConstraint("key", true)
                    searchObj.dialog.setConstraint("comment", true)
                    check(engineRoles() === "song,artist,key,comment,bpm", "s23_boot1_fields_active", "roles=" + engineRoles())
                    setSearchAll(true)
                    step = 3
                    tick.interval = 250
                    tick.start()
                    return
                }
                if (step === 3) {
                    if (root.scUiPending > 0 || root.scSearchFieldsRetry) {
                        if (++waits > 30) { check(false, "save_drain_timeout", "pending=" + root.scUiPending + " retry=" + root.scSearchFieldsRetry); finish(); return }
                        tick.interval = 200
                        tick.start()
                        return
                    }
                    check(root.scUiRev > revBeforeSave && rolesCanonical(root.scSearchFields) === "song,artist,key,comment,bpm",
                        "s23_boot1_save_confirmed", "rev=" + revBeforeSave + "->" + root.scUiRev + " fields=" + rolesCanonical(root.scSearchFields))
                    check(getSearchAll() === true, "s23_boot1_searchAll_independent_on")
                    step = 4
                    tick.interval = 400
                    tick.start()
                    return
                }
                finish()
            } else if (mode === "s23_boot2") {
                if (step === 0) {
                    if (!root.scUiLoaded || !root.scUiEpoch || root.scUiRev < 1 || !root.scSearchFieldsRestored) {
                        if (++waits > 50) { check(false, "boot2_ui_load_timeout", "loaded=" + root.scUiLoaded + " rev=" + root.scUiRev); finish(); return }
                        root.scCall("")
                        tick.interval = 250
                        tick.start()
                        return
                    }
                    waits = 0
                    check(engineRoles() === "song,artist,key,comment,bpm", "s23_boot2_restored_controller", "roles=" + engineRoles() + " epoch=" + root.scUiEpoch + " rev=" + root.scUiRev)
                    const sa = getSearchAll()
                    if (board === "JP11") {
                        check(sa === true, "s23_boot2_searchAll_preserved", "searchAll=" + sa)
                    } else {
                        console.warn("S23REAL INFO|" + board + "|" + mode + "|searchAll=" + sa + "|")
                    }
                    const pBrowse = Planck.getProperty("/GUI/ShowBrowseView")
                    if (pBrowse && pBrowse.translator) pBrowse.translator.state = true
                    const pBox = Planck.getProperty("/Client/Librarian/SearchBoxOpen")
                    if (pBox && pBox.translator) pBox.translator.state = true
                    step = 1
                    tick.interval = 350
                    tick.start()
                    return
                }
                if (step === 1) {
                    if (!findSearch()) {
                        if (++waits > 20) { check(false, "boot2_search_not_found"); finish(); return }
                        tick.interval = 200
                        tick.start()
                        return
                    }
                    check(objRolesCanonical(searchObj.searchConstraints) === "song,artist,key,comment,bpm",
                        "s23_boot2_search_qml_synced", "constraints=" + objRolesCanonical(searchObj.searchConstraints))
                    refineBtn.clicked()
                    step = 2
                    tick.interval = 250
                    tick.start()
                    return
                }
                if (step === 2) {
                    check(!!searchObj.dialog && objRolesCanonical(searchObj.dialog.searchConstraint) === "song,artist,key,comment,bpm",
                        "s23_boot2_dialog_synced", "dialog=" + (searchObj.dialog ? objRolesCanonical(searchObj.dialog.searchConstraint) : "null"))
                    revBeforeReset = root.scUiRev
                    const pReset = Planck.getProperty("/Client/Preferences/ResetAll")
                    check(!!(pReset && pReset.translator), "resetAll_prop_found")
                    if (pReset && pReset.translator) pReset.translator.state = true
                    step = 3
                    tick.interval = 300
                    tick.start()
                    return
                }
                if (step === 3) {
                    if (root.scUiPending > 0 || root.scSearchFieldsRetry) {
                        if (++waits > 30) { check(false, "reset_drain_timeout", "pending=" + root.scUiPending + " retry=" + root.scSearchFieldsRetry); finish(); return }
                        tick.interval = 200
                        tick.start()
                        return
                    }
                    check(engineRoles() === "song,artist", "s23_boot2_reset_controller", "roles=" + engineRoles())
                    check(objRolesCanonical(searchObj.searchConstraints) === "song,artist", "s23_boot2_reset_search_qml", "constraints=" + objRolesCanonical(searchObj.searchConstraints))
                    check(root.scUiRev > revBeforeReset && rolesCanonical(root.scSearchFields) === "song,artist",
                        "s23_boot2_reset_persisted", "rev=" + revBeforeReset + "->" + root.scUiRev)
                    step = 4
                    tick.interval = 400
                    tick.start()
                    return
                }
                finish()
            } else if (mode === "s23_boot3") {
                if (step === 0) {
                    if (!root.scUiLoaded || !root.scUiEpoch || root.scUiRev < 1 || !root.scSearchFieldsRestored) {
                        if (++waits > 50) { check(false, "boot3_ui_load_timeout", "loaded=" + root.scUiLoaded + " rev=" + root.scUiRev); finish(); return }
                        root.scCall("")
                        tick.interval = 250
                        tick.start()
                        return
                    }
                    check(engineRoles() === "song,artist", "s23_boot3_default_after_reset", "roles=" + engineRoles() + " epoch=" + root.scUiEpoch + " rev=" + root.scUiRev)
                    finish()
                }
            } else {
                check(false, "unknown_mode", mode)
                finish()
            }
        } catch (e) {
            fails++
            console.warn("S23REAL EXC|" + board + "|" + mode + "|" + e + "|")
            finish()
        }
    }
    Timer {
        id: tick
        interval: 300
        repeat: false
        running: true
        onTriggered: probe.runStep()
    }
}
