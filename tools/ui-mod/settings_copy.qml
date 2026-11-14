    // T7: local screen consent and the stock native profile loader.
    property var scCopyState: ({peers: [], nativeTypes: {}, operation: {}})
    property var scCopyRequests: []
    property bool scCopyStatusBusy: false
    property bool scCopyPeerBusy: false
    property bool scCopyPreviewAsked: false
    property string scCopySeenIncoming: ""
    property string scCopySeenDone: ""
    property string scCopyNativeID: ""
    property int scCopyLoadStage: 0
    property double scCopyLoadDeadline: 0
    property bool scCopyUSBLoading: false
    property bool scCopyReturning: false
    property double scCopyReturnUntil: 0
    property string scCopySaveDrive: ""
    property string scCopyClearedUSBID: ""
    property double scCopyLoadStarted: 0
    Repeater {
        id: scCopySources
        model: Planck.librarianController ? Planck.librarianController.browserSourceControllerPtr().connectedSourceModel() : null
        delegate: Item { property string label: String(model.name || "") }
    }
    function scCopyEscape(text) {
        return String(text || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/\n/g, "<br>")
    }
    function scCopyNotice(message) {
        PlanckGUI.views.prompts.post({title: scT("Settings copy"), message: scCopyEscape(scT(message)), options: Prompt.Ok})
    }
    function scCopyRequest(action, fields, done) {
        const x = new XMLHttpRequest()
        const request = {x: x, at: Date.now(), done: false, callback: done}
        scCopyRequests.push(request)
        x.onreadystatechange = function() {
            if (x.readyState !== 4 || request.done) return
            request.done = true
            let value = null
            try { value = JSON.parse(x.responseText) } catch (e) {}
            if (done) done(x.status === 200 ? value : null, value && value.error ? String(value.error) : scT("Settings request failed"))
        }
        x.open(action ? "POST" : "GET", "http://127.0.0.1:8080/v1/sc5000/settings-copy")
        if (action) {
            x.setRequestHeader("Content-Type", "application/json")
            x.setRequestHeader("Authorization", "Bearer " + root.scAccessCode)
            const body = fields || {}; body.action = action
            x.send(JSON.stringify(body))
        } else x.send()
    }
    function scCopyCapture() {
        const values = {}, entries = {}, types = scCopyState.nativeTypes || {}
        const motor = Config.device.hasMotorizedPlatters === true
        for (const key of Object.keys(types)) {
            if (!motor && ["MotorizedPlatterTorque", "TrackSpeed", "TurntableMode"].indexOf(key) >= 0) continue
            const p = Planck.getProperty("/Client/Preferences/Profile/Application/" + key)
            if (!p || !p.translator) continue
            const t = p.translator, kind = types[key]
            if (kind === 1) { values[key] = {type: 1, state: !!t.state}; entries[key] = [] }
            else if (kind === 4) {
                values[key] = {type: 4, string: String(t.string)}
                entries[key] = Array.from(t.entries).map(v => String(v))
            } else if (kind === 0) { values[key] = {type: 0, value: Number(t.unnormalized)}; entries[key] = [] }
        }
        const identity = {}
        const player = Planck.getProperty("/Client/Preferences/Player")
        if (player && player.translator) identity.Player = player.translator.index
        for (const key of ["Name", "Product", "PlayerColor1A", "PlayerColor1B", "PlayerColor2A", "PlayerColor2B", "PlayerColor3A", "PlayerColor3B", "PlayerColor4A", "PlayerColor4B"]) {
            const p = Planck.getProperty("/Client/Preferences/Profile/Application/" + key)
            if (p && p.translator) identity[key] = key.indexOf("PlayerColor") === 0 ? String(p.translator.color) : String(p.translator.string)
        }
        return {native: values, entries: entries, identity: identity}
    }
    function scCopyPlaying() {
        for (const d of root.scDeckList()) {
            const p = Planck.getProperty("/Engine/Deck" + d + "/PlayState")
            if (!p || !p.translator) return true
            if (p.translator.state) return true
        }
        return false
    }
    function scCopyCurrentDrive() { return String(Planck.librarianController.devicesController.getCurrentDevicePath() || "") }
    function scCopySelect(label) {
        const bsc = Planck.librarianController.browserSourceControllerPtr()
        for (let i = 0; i < scCopySources.count; i++) {
            if (scCopySources.itemAt(i).label === label) {
                bsc.select(bsc.connectedSourceModel().index(i, 0)); return true
            }
        }
        return false
    }
    function scCopyBegin(peer) {
        scCopyPreviewAsked = false
        scCopyRequest("begin", {peer: peer}, function(ok, error) { if (!ok) scCopyNotice(error); else scCopyNotice(scT("Approve the request on the source deck")) })
    }
    function scCopySummary(value) {
        const skipped = (value.skippedNative || []).concat(value.skippedUI || [])
        return scT("Stock preferences") + ": " + (value.native || []).length + "\n" +
               scT("EngineX settings") + ": " + (value.ui || []).length +
               (skipped.length ? "\n" + scT("Skipped settings") + ": " + skipped.join(", ") : "")
    }
    function scCopyConfirm(preview, undo, failedImport) {
        if (scCopyPlaying()) { scCopyNotice(scT("Pause playback before copying or undoing settings")); return }
        const text = (preview ? scCopySummary(preview) + "\n\n" : "") +
                     scT("Engine will restart and playback will stop.") + "\n" +
                     scT("Player number, role, colours and identity stay on this deck.")
        const prompt = PlanckGUI.views.prompts.post({title: scT(undo ? "Undo last copy" : "Copy settings"), message: scCopyEscape(text), options: Prompt.Cancel | Prompt.Ok})
        prompt.finished.connect(function(role) {
            if (role !== Prompt.AcceptRole) {
                if (!undo) scCopyRequest("cancel", {}, function() {})
                return
            }
            if (scCopyPlaying()) { scCopyNotice(scT("Pause playback before copying or undoing settings")); return }
            const body = scCopyCapture()
            body.bundleHash = preview ? preview.bundleHash : ""
            body.confirmed = true; body.playing = false; body.previousDrive = scCopyCurrentDrive()
            scCopyRequest(undo ? (failedImport ? "rollback" : "undo") : "apply", body, function(ok, error) {
                if (!ok) { scCopyRequest("cancel", {}, function() {}); scCopyNotice(error); return }
                if (scCopyPlaying()) {
                    scCopyRequest("cancel", {}, function() {})
                    scCopyNotice(scT("Pause playback before copying or undoing settings")); return
                }
                Planck.quit(QuitReasons.AnalyticsChange, true)
            })
        })
    }
    function scCopyUndo() { scCopyConfirm(null, true, !!scCopyState.rollback) }
    function scCopySourcePrompt(incoming) {
        scCopySeenIncoming = incoming.token
        const prompt = PlanckGUI.views.prompts.post({title: scT("Allow settings copy?"),
            message: scCopyEscape(scT("Allow this deck to read your preferences once?") + "\n" + incoming.peer), options: Prompt.No | Prompt.Yes})
        prompt.finished.connect(function(role) {
            const body = role === Prompt.AcceptRole ? scCopyCapture() : {}
            body.token = incoming.token
            scCopyRequest(role === Prompt.AcceptRole ? "allow" : "deny", body, function(ok, error) { if (!ok) { scCopySeenIncoming = ""; scCopyNotice(error) } })
        })
    }
    function scCopyReceivePreview() {
        scCopyPreviewAsked = true
        scCopyRequest("preview", scCopyCapture(), function(preview, error) {
            if (!preview) { scCopyNotice(error); return }
            scCopyConfirm(preview, false, false)
        })
    }
    function scCopyFailure(message) {
        scCopyLoadStage = 0
        scCopySeenDone = scCopyNativeID
        scCopyRequest("failed", {id: scCopyNativeID, loading: !!Planck.getProperty("/Client/Preferences/Profile/LoadApplicationProfile").translator.state}, function(ok) {
            scCopyNotice(message)
            if (ok) scCopyConfirm(null, true, true)
        })
    }
    function scCopyLoadTick() {
        const op = scCopyState.operation || {}
        if (!scCopyLoadStage) return
        if (scCopyLoadStage <= 2 && Date.now() > scCopyLoadDeadline) { scCopyFailure(scT("Engine did not finish loading the profile")); return }
        const load = Planck.getProperty("/Client/Preferences/Profile/LoadApplicationProfile").translator
        if (scCopyLoadStage === 1) {
            if (scCopyPlaying()) return
            if (scCopyCurrentDrive() !== scCopyState.drive) { scCopySelect("EngineX-Settings"); return }
            load.state = true
            scCopyLoadStarted = Date.now()
            scCopyLoadStage = 2
            return
        }
        if (scCopyLoadStage === 2) {
            if (load.state || Date.now() - scCopyLoadStarted < 500) return
            scCopyLoadStage = 3
            const capture = scCopyCapture(); capture.id = scCopyNativeID
            scCopyRequest("finish", capture, function(ok, error) {
                if (scCopyLoadStage !== 3) return
                if (!ok) { scCopyFailure(error); return }
                root.scCall()
                scCopyReturning = op.operation !== "usb"
                scCopyReturnUntil = Date.now() + 3000
                if (scCopyReturning && op.previousDrive) scCopySelect(op.previousDrive.split("/").pop())
                scCopyLoadStage = 4
            })
            return
        }
        if (scCopyLoadStage === 4) {
            if (scCopyReturning) Planck.librarianController.devicesController.ejectDevice(scCopyState.drive)
            scCopyLoadStage = 0; scCopyUSBLoading = false
        }
    }
    function scCopyPoll() {
        const now = Date.now()
        for (const request of scCopyRequests) if (!request.done && now - request.at > 12000) {
            request.done = true; request.x.abort()
            if (request.callback) request.callback(null, scT("Settings request timed out"))
        }
        scCopyRequests = scCopyRequests.filter(request => !request.done)
        if (scCopyStatusBusy) return
        scCopyStatusBusy = true
        scCopyRequest("", {}, function(state) {
            scCopyStatusBusy = false
            if (!state) return
            scCopyState = state
            if (state.incoming && state.incoming.token !== scCopySeenIncoming) scCopySourcePrompt(state.incoming)
            if (state.outgoing && !scCopyPeerBusy) {
                scCopyPeerBusy = true
                scCopyRequest("poll", {}, function(ok, error) { scCopyPeerBusy = false; if (!ok) scCopyNotice(error) })
            }
            if (state.received && !scCopyPreviewAsked) scCopyReceivePreview()
            const op = state.operation || {}
            if (scCopyLoadStage > 0 && op.phase !== "load" && op.phase !== "usb-load" && op.phase !== "done") scCopyLoadStage = 0
            if ((op.phase === "load" || (op.phase === "usb-load" && scCopyUSBLoading)) && scCopyNativeID !== op.id) {
                scCopyNativeID = op.id
                scCopyLoadDeadline = Date.now() + 15000
                scCopyLoadStarted = Date.now()
                scCopyLoadStage = op.phase === "usb-load" ? 2 : 1
            }
            if (op.phase === "done" && scCopyNativeID === op.id && scCopySeenDone !== op.id) {
                scCopySeenDone = op.id
                if (op.operation === "undo" || op.operation === "rollback") scCopyNotice(scT("Previous settings restored"))
                else scCopyNotice(scT("Settings copied"))
            }
            if (op.phase === "failed" && op.operation === "usb" && scCopyClearedUSBID !== op.id && !Planck.getProperty("/Client/Preferences/Profile/LoadApplicationProfile").translator.state) {
                scCopyUSBLoading = false
                scCopyClearedUSBID = op.id
                scCopyRequest("failed", {id: op.id, loading: false}, function(ok) { if (!ok) scCopyClearedUSBID = "" })
            }
            if (op.phase === "failed" && scCopySeenDone !== op.id) { scCopySeenDone = op.id; scCopyNotice(op.error) }
        })
    }
    Timer { interval: 1000; repeat: true; running: true; onTriggered: root.scCopyPoll() }
    Timer { interval: 200; repeat: true; running: root.scCopyLoadStage > 0; onTriggered: root.scCopyLoadTick() }
    function scCopyUSBPrompt(dialog, show, finished, loadPreferences, extras) {
        const drive = scCopyCurrentDrive()
        const op = scCopyState.operation || {}
        const finish = function() { show.translator.state = false; finished.translator.state = true }
        if (op.phase === "load" || (scCopyReturning && Date.now() < scCopyReturnUntil && drive === op.previousDrive)) { finish(); return }
        const body = scCopyCapture(); body.drive = drive
        scCopyRequest("usb-preview", body, function(preview, error) {
            if (!preview) { finish(); scCopyNotice(error); return }
            dialog.scCopyIncludeAvailable = !!preview.includeEngineXAvailable
            dialog.scCopyInclude = dialog.scCopyIncludeAvailable
            const prompt = PlanckGUI.views.prompts.post({id: "LoadPreferencesDialog", title: dialog.messageText,
                message: scCopyEscape(scCopySummary(preview)), options: Prompt.No | Prompt.Yes})
            prompt.extras = extras
            prompt.finished.connect(function(role) {
                if (role !== Prompt.AcceptRole) { finish(); return }
                const apply = scCopyCapture()
                apply.drive = drive; apply.includeEngineX = dialog.scCopyInclude
                apply.profileHash = preview.profileHash; apply.confirmed = true
                scCopyRequest("usb-prepare", apply, function(ok, failure) {
                    if (!ok) { finish(); scCopyNotice(failure); return }
                    scCopyUSBLoading = true
                    loadPreferences()
                    finish()
                })
            })
        })
    }
    function scCopyExportDrive() {
        const drive = scCopySaveDrive
        scCopySaveDrive = ""
        if (!drive || drive === scCopyState.drive) return
        scCopyRequest("usb-export", {drive: drive}, function(ok, error) { if (!ok) scCopyNotice(error) })
    }
