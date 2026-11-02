#!/usr/bin/env python3
"""Build the UI packs engine_ui_hook.c loads into Engine (see README.md)."""
import argparse
import json
import re
import shutil
import tarfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = "Skins/EmbeddedCommon/"
DEV_HARNESS = """
	Timer {
		interval: 500; running: true; repeat: true
		property string last: ""
		onTriggered: {
			const x = new XMLHttpRequest()
			x.open("GET", "file:///tmp/ui-eval.js")
			x.onreadystatechange = function() {
				if (x.readyState !== 4 || !x.responseText) return
				const n = x.responseText.indexOf("\\n")
				const id = x.responseText.slice(0, n)
				if (id === last) return
				last = id
				try { console.warn("SCEVAL " + id + " = " + eval(x.responseText.slice(n + 1))) }
				catch (e) { console.warn("SCEVAL " + id + " ERR " + e + " " + e.lineNumber) }
			}
			x.send()
		}
	}
"""


def patch(text: str, old: str, new: str, path: str) -> str:
    if text.count(old) != 1:
        raise SystemExit(f"{path}: expected exactly one match for {old!r}, got {text.count(old)}")
    return text.replace(old, new)


SERVICES_MAIN = """
	// SC5000 UI mod: settings owned by stream-sync (see pkg/webui/ui_settings.go).
	property string scLang: "en"
	property string scWave: "denon"
	property string scQuality: "flac"
	property var scSources: ["tidal", "netease", "spotify", "deezer", "soundcloud"]
	property var scLoggedIn: []
	property string scScope: ""
	readonly property var scServices: [
		{ id: "tidal", name: "TIDAL" },
		{ id: "netease", name: scT("NetEase Cloud Music") },
		{ id: "spotify", name: "Spotify" },
		{ id: "deezer", name: "Deezer" },
		{ id: "soundcloud", name: "SoundCloud" }
	]
	readonly property var scDict: (@DICT@)
	function scT(s) { return scDict[s] || s }
	function scIcon(id) {
		if (id === "tidal") return "qrc:/UIKit.1/UIKit/Icons/SourceTIDALColored.png"
		if (id === "soundcloud") return "qrc:/UIKit.1/UIKit/Icons/SourceSoundCloudColored.png"
		return "file:///data/sc5000-custom/ui/icons/" + id + ".png"
	}
	// The gateway appends " [128 BPM 8A]" to a row's label; see labelWithBPM. A value
	// Engine analysed itself always wins, so these only fill empty columns.
	function scPreview(label) {
		return /\\s\\[(\\d+) BPM(?: ([0-9]{1,2}[AB]))?\\]$/.exec(label || "") || ["", "", ""]
	}
	function scPlainTitle(song) { return (song || "").replace(/\\s\\[\\d+ BPM(?: [0-9]{1,2}[AB])?\\]$/, "") }
	// Engine's messages name its one streaming client; say which service is showing.
	function scBrand(s) {
		const service = scServices.find(v => v.id === scScope)
		return s && service && scScope !== "tidal" ? String(s).replace(/TIDAL/g, service.name) : s
	}
	function scCall(query, done) {
		const x = new XMLHttpRequest()
		x.open(query ? "POST" : "GET", "http://127.0.0.1:8080/v1/sc5000/ui" + (query ? "?" + query : ""))
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			scApply(JSON.parse(x.responseText))
			if (done) done()
		}
		x.send()
	}
	function scApply(v) {
		scEnsureTidal()
		scLang = v.language
		scWave = v.waveform
		scQuality = v.quality || "flac"
		scSources = v.sources
		scLoggedIn = v.loggedIn
		scScope = v.scope
		const p = Planck.getProperty("/Client/Preferences/Profile/Application/WaveformStyle")
		if (p.translator && (p.translator.index == 1) !== (v.waveform === "rgb"))
			p.translator.index = v.waveform === "rgb" ? 1 : 0
	}
	function scSetSource(id, on) {
		const list = scSources.filter(s => s !== id)
		if (on) list.push(id)
		scCall("sources=" + list.join(","))
	}
	// Every service is backed by Engine's TIDAL provider; the gateway follows the scope.
	function scOpen(id) {
		scEnsureTidal()
		scCall("scope=" + id, function() {
			Planck.librarianController.streamingPlaylistsController.invalidateUI()
			scSelectPending = !scSelectTidal()
			scEnterTries = 8
			scEnterTimer.start()
			if (id !== "tidal" && scLoggedIn.indexOf(id) < 0) scLoginBegin(id)
		})
	}
	property string scLogin: ""
	property string scLoginStatus: ""
	property int scLoginSerial: 0
	function scLoginRequest(method, id, done) {
		const x = new XMLHttpRequest()
		x.open(method, "http://127.0.0.1:8080/v1/sc5000/login?provider=" + id)
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || scLogin !== id) return
			if (x.status === 200) done(JSON.parse(x.responseText).status)
			else scLoginStatus = "error"
		}
		x.send()
	}
	function scLoginBegin(id) {
		scLogin = id
		scLoginStatus = "starting"
		scLoginRequest("POST", id, function(status) {
			scLoginSerial++
			scLoginStatus = status
		})
	}
	function scLoginDone() {
		scLogin = ""
		scCall("", function() {
			Planck.librarianController.streamingPlaylistsController.invalidateUI()
			scEnterTries = 8
			scEnterTimer.start()
		})
	}
	Timer {
		interval: 2000; repeat: true
		running: scLogin !== "" && scLoginStatus !== "starting" && scLoginStatus !== "error"
		onTriggered: {
			const id = scLogin
			scLoginRequest("GET", id, function(status) {
				if (status === "done") scLoginDone()
				else if (status === "expired") scLoginBegin(id)
				else scLoginStatus = status
			})
		}
	}
	// The playlists are one level down; open them so a source never looks empty.
	property int scEnterTries: 0
	property bool scSelectPending: false
	// The row may not exist yet right after the provider is re-enabled, and a
	// delegate can be recycled, so find it in the model rather than the view.
	function scSelectTidal() {
		const sources = Planck.librarianController.browserSourceControllerPtr()
		const model = sources.model()
		for (let i = 0; i < model.rowCount(); i++) {
			const index = model.index(i, 0)
			if (model.data(index, scSourceNameRole) === "TIDAL") {
				sources.select(index)
				return true
			}
		}
		return false
	}
	readonly property int scSourceNameRole: 257
	// Every service depends on Engine's TIDAL provider. A saved profile can turn it
	// off, which would leave all four service rows doing nothing.
	function scEnsureTidal() {
		for (let i = 0; i < scProviders.count; i++) {
			const watcher = scProviders.objectAt(i)
			if (watcher) watcher.ensure()
		}
	}
	function scFindMyPlaylists(item) {
		if (item.objectName === "Browser_Collection_StackView_ViewDelegate" && item.visible && item.modelData && item.modelData.display === "My Playlists")
			return item
		for (let i = 0; item.children && i < item.children.length; i++) {
			const found = scFindMyPlaylists(item.children[i])
			if (found) return found
		}
		return null
	}
	Timer {
		id: scEnterTimer
		interval: 300; repeat: true
		onTriggered: {
			if (scSelectPending) scSelectPending = !scSelectTidal()
			const row = scFindMyPlaylists(uiContainer)
			if (row) row.clicked(null)
			if (row || --scEnterTries <= 0) stop()
		}
	}
	Timer {
		interval: 3000; running: true; repeat: true; triggeredOnStart: true
		onTriggered: scCall("")
	}
	Instantiator {
		id: scProviders
		model: Planck.librarianController.streamingInterfaceController.providerModel
		delegate: QtObject {
			function ensure() { if (provider.name === "TIDAL" && !provider.integrated) provider.integrated = true }
			readonly property Connections watcher: Connections {
				target: provider
				function onIntegratedChanged() { ensure() }
			}
			Component.onCompleted: ensure()
		}
	}
"""

LOGIN_DIALOG = """
		Rectangle {
			anchors.fill: parent
			z: 1000000
			visible: root.scLogin !== ""
			color: "#e0000000"
			readonly property var service: root.scServices.find(s => s.id === root.scLogin)
			MouseArea { anchors.fill: parent }
			Rectangle {
				anchors.centerIn: parent
				width: 1040; height: 660
				radius: 24
				color: Style.surface02Color
				Rectangle {
					x: 40; y: 60
					width: 540; height: 540
					radius: 16
					color: "white"
					Image {
						anchors.centerIn: parent
						width: 508; height: 508
						smooth: false
						cache: false
						visible: root.scLoginSerial > 0 && root.scLoginStatus !== "error"
						source: root.scLogin && root.scLoginSerial > 0 ? "http://127.0.0.1:8080/v1/sc5000/login/qr?provider=" + root.scLogin + "&n=" + root.scLoginSerial : ""
					}
				}
				Image {
					x: 620; y: 70
					width: 88; height: 88
					mipmap: true
					source: root.scLogin ? root.scIcon(root.scLogin) : ""
				}
				Text {
					x: 620; y: 180
					width: 380
					text: parent.parent.service ? parent.parent.service.name : ""
					color: Style.font02Color
					font: Style.fontH0
				}
				Text {
					x: 620; y: 240
					width: 380
					wrapMode: Text.WordWrap
					color: Style.font02Color
					font: Style.fontH1
					text: {
						if (root.scLoginStatus === "error") return root.scT("Can't start login. Check the network.")
						if (root.scLoginStatus === "scanned") return root.scT("Confirm on your phone")
						if (root.scLogin === "netease") return root.scT("Scan with the NetEase Cloud Music app")
						return root.scT("Scan with your phone camera.")
					}
				}
				Rectangle {
					x: 620; y: 520
					width: 380; height: 80
					radius: 16
					color: closeArea.pressed ? Style.focus02Color : Style.surface03Color
					Text {
						anchors.centerIn: parent
						text: root.scT("Not now")
						color: Style.font02Color
						font: Style.fontH1
					}
					MouseArea {
						id: closeArea
						anchors.fill: parent
						onClicked: root.scLogin = ""
					}
				}
			}
		}
"""

SOURCE_FOOTER = """
	property var scTidalRow: null
	footer: Column {
		width: connectedSource.width
		Repeater {
			model: root.scServices.filter(s => root.scSources.indexOf(s.id) >= 0)
			MouseArea {
				width: connectedSource.width
				height: 108
				onClicked: root.scOpen(modelData.id)
				Rectangle {
					anchors.fill: parent
					radius: 16
					color: parent.pressed ? Style.focus02Color : Style.surface02Color
					border.color: Style.icon02Color
					border.width: connectedSource.scTidalRow && connectedSource.scTidalRow.isCurrent && root.scScope === modelData.id ? 2 : 0
				}
				Image {
					x: connectedSource.delegateLeftPadding + 8
					anchors.verticalCenter: parent.verticalCenter
					width: 48; height: 48
					mipmap: true
					source: root.scIcon(modelData.id)
				}
				Text {
					x: connectedSource.delegateLeftPadding + 80
					anchors.verticalCenter: parent.verticalCenter
					text: modelData.name
					color: Style.font02Color
					font: Style.fontH1
				}
				MouseArea {
					anchors { right: parent.right; top: parent.top; bottom: parent.bottom }
					width: 140
					visible: modelData.id !== "tidal"
					onClicked: root.scLoginBegin(modelData.id)
					Text {
						anchors.centerIn: parent
						text: root.scT(root.scLoggedIn.indexOf(modelData.id) >= 0 ? "Switch account" : "Scan to log in")
						color: Style.font02Color
						font: Style.fontH3
					}
				}
			}
		}
	}
"""

AVAILABLE_TILES = """
						Flow {
							Layout.fillWidth: true
							spacing: availableSource.cellPadding
							visible: children.length > 1
							Repeater {
								model: root.scServices.filter(s => root.scSources.indexOf(s.id) < 0)
								MouseArea {
									width: 180; height: 180
									onClicked: root.scSetSource(modelData.id, true)
									Rectangle { anchors.fill: parent; radius: 24; color: parent.pressed ? Style.focus02Color : Style.surface03Color }
									Image { anchors.horizontalCenter: parent.horizontalCenter; y: 36; width: 48; height: 48; mipmap: true; source: root.scIcon(modelData.id) }
									Text { anchors.horizontalCenter: parent.horizontalCenter; y: 110; text: modelData.name; color: Style.font02Color; font: Style.fontH1 }
								}
							}
						}
"""

STREAM_SWITCHES = """
	Repeater {
		model: root.scServices
		SwitchEntry {
			text: modelData.name
			icon.source: root.scIcon(modelData.id)
			icon.useOriginal: true
			checked: root.scSources.indexOf(modelData.id) >= 0
			onClicked: root.scSetSource(modelData.id, !(root.scSources.indexOf(modelData.id) >= 0))
		}
	}
"""

WAVEFORM_ENTRY = """import QtQuick 2.12
import StateMap 1.0
import Planck 1.0
import "Base"
ListEntry {
	text: "Waveform Style"
	readonly property var ids: ["denon", "rgb", "3band"]
	values: ["Denon", "RGB", "3-Band"]
	currentValue: values[Math.max(0, ids.indexOf(root.scWave))]
	onSelected: function(index) {
		if (index === 1 && root.scWave !== "rgb") {
			PlanckGUI.views.prompts.post({
				type: Prompt.Default,
				title: "RGB Waveform Creation",
				message: "RGB Waveforms and Track Overviews will be built when loading to deck.",
				options: Prompt.Ok
			})
		}
		root.scCall("waveform=" + ids[index])
	}
}
"""

QUALITY_ENTRY = """
		EntryBase.ListEntry {
			text: "Streaming Quality"
			readonly property var ids: ["flac", "320k"]
			values: ["FLAC", "320kbps"]
			currentValue: values[Math.max(0, ids.indexOf(root.scQuality))]
			onSelected: function(index) { root.scCall("quality=" + ids[index]) }
		},
"""

LANGUAGE_ENTRY = """
		EntryBase.ListEntry {
			text: "Language"
			readonly property var ids: ["en", "zh-Hans", "zh-Hant"]
			values: ["English", "简体中文", "繁體中文"]
			currentValue: values[Math.max(0, ids.indexOf(root.scLang))]
			onSelected: function(index) {
				if (ids[index] === root.scLang) return
				const lang = ids[index]
				const prompt = PlanckGUI.views.prompts.post({
					type: Prompt.Warning,
					message: "切换语言需要重启 Engine，播放会中断。\nRestart Engine to change language? Playback will stop.",
					options: Prompt.Cancel | Prompt.Ok
				})
				prompt.finished.connect(function(role) {
					if (role === Prompt.AcceptRole) root.scCall("lang=" + lang + "&restart=1")
				})
			}
		},
		WaveformStyle { },
"""

BAND_COLORS = {"Low": "#1a5cff", "Mid": "#ff9416", "High": "#ffffff"}  # Pioneer 3-Band


def three_band(style_name, band):
    return f'(root.scWave === "3band" ? "{BAND_COLORS[band]}" : Style.{style_name})'


def edits(dev: bool):
    v = ROOT + "Views/"
    prefs = v + "ControlCenter/Tabs/Preferences/"
    wave = [(f"rgbMode ? Style.waveformRGB{b}Color : Style.waveform{b}FadeColor", f"rgbMode ? Style.waveformRGB{b}Color : {three_band(f'waveform{b}FadeColor', b)}") for b in BAND_COLORS]
    wave += [(f"color{b}Color: Style.waveform{b}Color", f"color{b}Color: {three_band(f'waveform{b}Color', b)}") for b in BAND_COLORS]
    overview = [(f"rgbMode ? Style.waveformRGB{b}Color : Style.waveform{b}Color", f"rgbMode ? Style.waveformRGB{b}Color : {three_band(f'waveform{b}Color', b)}") for b in BAND_COLORS]
    main = DEV_HARNESS if dev else ""
    return {
        # The flag is still cleared, which is what pressing "Ok" would do.
        ROOT + "Items/MessageBoxes/ImproperDeviceEjectMessageBox.qml": [(
            "      let messageObject = {", "      Planck.librarianController.deviceEjectedImproperly = false\n      return\n      let messageObject = {")],
        ROOT + "EmbeddedMain.qml": [
            ("	Connections {\n		target: MainWindow\n", SERVICES_MAIN + main + "\n	Connections {\n		target: MainWindow\n"),
            ("		id: uiContainer\n", "		id: uiContainer\n" + LOGIN_DIALOG),
        ],
        v + "Common/Waveform/Waveform.qml": wave,
        v + "Common/WaveformOverview/WaveformOverview.qml": overview,
        v + "Common/WaveformOverview/WaveformOverviewLibrary.qml": overview,
        prefs + "Entries/WaveformStyle.qml": WAVEFORM_ENTRY,
        prefs + "Entries/Base/ListEntry.qml": [
            ("function isSelected(text) {", "function isSelected(value) {"),
            ("return entry.currentValue === text\n", "return entry.currentValue === value\n"),
            ("entry.currentValue.includes(text)", "entry.currentValue.includes(value)"),
            ("					text: modelData\n", "					text: root.scT(modelData)\n"),
            ("down: pressed || isSelected(text)", "down: pressed || isSelected(modelData)"),
            ("if(isSelected(text) && entry.defaultIndex", "if(isSelected(modelData) && entry.defaultIndex"),
        ],
        prefs + "TabContents/DeviceTabContent.qml": [
            ('import "../Entries"\n', 'import "../Entries"\nimport "../Entries/Base" as EntryBase\nimport Planck 1.0\n'),
            ("		PlayerNumber { },\n", LANGUAGE_ENTRY + QUALITY_ENTRY + "		PlayerNumber { },\n"),
        ],
        prefs + "Entries/StreamingServices.qml": [
            ("		visible: !isCloudService && !isEaasService\n", '		visible: !isCloudService && !isEaasService && provider.name !== "TIDAL" && provider.name !== "SoundCloud"\n'),
            ('import "Base"\n', 'import QtQuick.Layouts 1.12\nimport "Base"\n'),
            ('	label: "Streaming"\n', '	label: "Streaming"\n' + STREAM_SWITCHES),
        ],
        v + "BrowserSource/ConnectedSource.qml": [
            ("	currentIndex: -1\n", "	currentIndex: -1\n" + SOURCE_FOOTER),
            ("		height: 108\n		leftPadding", '		height: model.name === "TIDAL" || model.name === "SoundCloud" ? 0 : 108\n		visible: model.name !== "TIDAL" && model.name !== "SoundCloud"\n		Component.onCompleted: if (model.name === "TIDAL") connectedSource.scTidalRow = this\n		leftPadding'),
        ],
        v + "ControlCenter/ControlCenterTabView.qml": [("text: tabView.hideTabButtonLabel ? \"\" : modelData", "text: tabView.hideTabButtonLabel ? \"\" : root.scT(modelData)")],
        prefs + "PreferencesTabView.qml": [("					text: modelData\n", "					text: root.scT(modelData)\n")],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbViewHeader.qml": [("text: breadcrumbViewHeader.title\n", 'text: ["My Playlists", "My Collection"].indexOf(breadcrumbViewHeader.title) >= 0 ? root.scT(breadcrumbViewHeader.title) : breadcrumbViewHeader.title\n')],
        v + "BrowserSource/SourceScreen.qml": [("						AvailableSource {\n", AVAILABLE_TILES + "						AvailableSource {\n")],
        v + "Streaming/StreamingDialog.qml": [
            # This limit is for TIDAL's own qualities; DJ sets here run for hours.
            ("		function onShowTrackTooLong() {\n", "		function onShowTrackTooLong() {\n			return\n"),
            ("		function onShowGenericError(errorMessage, title) {\n", "		function onShowGenericError(errorMessage, title) {\n			title = root.scBrand(title)\n			errorMessage = root.scBrand(errorMessage)\n"),
            ("		function onShowGenericMessage(message, title) {\n", "		function onShowGenericMessage(message, title) {\n			title = root.scBrand(title)\n			message = root.scBrand(message)\n"),
            ("`${provider.displayName}: ${message}`", "root.scBrand(`${provider.displayName}: ${message}`)"),
            ("`This may be another PRIME player downloading or another ${Planck.librarianController.streamingInterfaceController.selectedProviderName} client playing a track.`", "root.scBrand(`This may be another PRIME player downloading or another ${Planck.librarianController.streamingInterfaceController.selectedProviderName} client playing a track.`)"),
            ("`This will remove all saved waveform data, cues and loops for ${provider.displayName} tracks from the selected source drive. Continue?`", "root.scBrand(`This will remove all saved waveform data, cues and loops for ${provider.displayName} tracks from the selected source drive. Continue?`)"),
            ("`${provider.displayName} session is no longer valid. This may be due to a recent password change. In order to continue, please pair with ${provider.displayName} again.`", "root.scBrand(`${provider.displayName} session is no longer valid. This may be due to a recent password change. In order to continue, please pair with ${provider.displayName} again.`)"),
            ("`Are you sure you want to log out from ${provider.displayName}?`", "root.scBrand(`Are you sure you want to log out from ${provider.displayName}?`)"),
            ("`Please log out of your Engine DJ profile to log out of ${provider.displayName}.`", "root.scBrand(`Please log out of your Engine DJ profile to log out of ${provider.displayName}.`)"),
            ("`Logging out will remove all ${provider.displayName} tracks from deck. Are you sure you want to log out?`", "root.scBrand(`Logging out will remove all ${provider.displayName} tracks from deck. Are you sure you want to log out?`)"),
            ("`${provider.displayName} - ${brief}`", "root.scBrand(`${provider.displayName} - ${brief}`)"),
        ],
        v + "Streaming/StreamingAcceptDenyDialog.qml": [
            (
                '						width: 400\n						height: (src.height / src.width) * width\n						color: Style.icon01Color\n						source: service.provider ? `qrc:/streaming/${service.provider.providerType}/Logo.png` : ""\n',
                '						width: root.scScope && root.scScope !== "tidal" ? 120 : 400\n						height: root.scScope && root.scScope !== "tidal" ? 120 : (src.height / src.width) * width\n						useOriginal: !!(root.scScope && root.scScope !== "tidal")\n						color: Style.icon01Color\n						source: service.provider ? (root.scScope && root.scScope !== "tidal" ? root.scIcon(root.scScope) : `qrc:/streaming/${service.provider.providerType}/Logo.png`) : ""\n',
            ),
            (
                '						text: service.provider ? `Connecting to ${service.provider.displayName}` : ""\n',
                '						text: service.provider ? root.scT("Connecting to") + " " + root.scBrand(service.provider.displayName) : ""\n',
            ),
        ],
        v + "Streaming/StreamingUtilityDialog.qml": [
            (
                '			width: 400\n			height: (src.height / src.width) * width\n			color: Style.icon01Color\n			source: dialog.provider ? "qrc:/streaming/%1/Logo.png".arg(dialog.provider.providerType) : ""\n',
                '			width: root.scScope && root.scScope !== "tidal" ? 96 : 400\n			height: root.scScope && root.scScope !== "tidal" ? 96 : (src.height / src.width) * width\n			useOriginal: !!(root.scScope && root.scScope !== "tidal")\n			color: Style.icon01Color\n			source: dialog.provider ? (root.scScope && root.scScope !== "tidal" ? root.scIcon(root.scScope) : "qrc:/streaming/%1/Logo.png".arg(dialog.provider.providerType)) : ""\n',
            ),
        ],
        v + "Streaming/StreamingLoginDialog.qml": [
            (
                '					text: `Log in to ${loginDialog.provider ? loginDialog.provider.displayName: ""} to access your tracks and playlists`\n',
                '					text: root.scBrand(`Log in to ${loginDialog.provider ? loginDialog.provider.displayName: ""} to access your tracks and playlists`)\n',
            ),
        ],
        v + "BrowseView/Components/TrackList.qml": [
            (
                "	readonly property string selectedProviderDisplayName: Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName\n",
                "	readonly property string selectedProviderDisplayName: root.scBrand(Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName)\n",
            ),
        ],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbViewContentContainer.qml": [
            (
                "		providerName: Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName\n",
                "		providerName: root.scBrand(Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName)\n",
            ),
        ],
        v + "BrowseView/Components/Components/Bpm.qml": [('text: row.modelData.bpm || ""', 'text: row.modelData.bpm || root.scPreview(row.modelData.providerName)[1]')],
        v + "BrowseView/Components/Components/Key.qml": [('text: row.modelData.key || ""', 'text: row.modelData.key || root.scPreview(row.modelData.providerName)[2] || ""')],
        v + "BrowseView/Components/Components/Song.qml": [('title: row.modelData.song || ""', 'title: root.scPlainTitle(row.modelData.song)')],
        # rb-bridge converts rekordbox media at mount. Engine's own slow import of a
        # large library corrupted m.db, so always take the prompt's "Ignore" branch.
        ROOT + "Items/MessageBoxes/RekordBoxImportDialog.qml": [(
            "		function onRequestChanged(devicePath) {\n",
            "		function onRequestChanged(devicePath) {\n"
            "			Planck.librarianController.allowDeviceForNfs(devicePath)\n"
            "			Planck.librarianController.devicesController.triggerDeviceAutoSelection()\n"
            "			return\n",
        )],
        v + "BrowseView/Components/RetryingHint.qml": [
            (
                '		text: "WAITING FOR " + providerName.toUpperCase()\n',
                '		text: root.scT("WAITING FOR") + " " + root.scBrand(providerName).toUpperCase()\n',
            ),
        ],
        v + "Common/Sidebar/Sidebar.qml": [
            ("source: browserSource.sourceIconUrl\n", 'source: browserSource.sourceIconUrl.indexOf("TIDAL") >= 0 && root.scScope ? root.scIcon(root.scScope) : browserSource.sourceIconUrl\n'),
            ("useOriginal: browserSource.useOriginalIcon\n", 'useOriginal: browserSource.useOriginalIcon || browserSource.sourceIconUrl.indexOf("TIDAL") >= 0\n'),
        ],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml": [
            ('				objectName: "Browser_Collection_StackView_ViewDelegate"\n',
             '				objectName: "Browser_Collection_StackView_ViewDelegate"\n'
             '				readonly property bool scHidden: breadcrumbView.isStreamingContent && root.scScope !== "tidal" && contentView.StackView.index === 0 && !!modelData && (root.scScope === "netease" ? ["Featured", "Genres", "Moods", "TIDAL Rising"] : ["My Collection", "Featured", "Genres", "Moods", "TIDAL Rising"]).indexOf(modelData.display) >= 0\n'
             # The view assigns visible itself and would undo a binding. These rows
             # end the list, so what stays is blank space below My Playlists.
             '				opacity: scHidden ? 0 : 1\n'),
            ("				enabled: !stackView.busy\n				isDragged:", "				enabled: !stackView.busy && !scHidden\n				isDragged:"),
            ("						return modelData.display\n					}\n					return \"\"", "						return breadcrumbView.isStreamingContent && contentView.StackView.index === 0 ? root.scT(modelData.display) : modelData.display\n					}\n					return \"\""),
        ],
    }


# Only these properties hold display text. Ids such as StateMap names never match.
TEXT_LINE = re.compile(r'^(\s*(?:[\w.]*\.)?(?:text|label|title|message|headline|info|display|description|missingContentHint)\s*:)(.*)$')
BARE_LITERAL = re.compile(r'^(\s*)"([^"\\]+)"(\s*)$')
LITERAL = re.compile(r'"([^"\\\n]+)"')


def load_dict():
    rows = {}
    for line in (HERE / "i18n.tsv").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            en, hans, hant = line.split("\t")
            rows[en] = {"zh-Hans": hans, "zh-Hant": hant}
    return rows


def compared_strings(qrc: Path):
    """Literals used in comparisons drive logic, so they must stay in English."""
    found = set()
    for f in qrc.rglob("*.qml"):
        found |= set(re.findall(r'[=!]==?\s*"([^"\n]+)"', f.read_text(encoding="utf-8", errors="replace")))
    return found


def translate(text, table, skip):
    def literal(m):
        return f'"{table[m.group(1)]}"' if m.group(1) in table and m.group(1) not in skip else m.group(0)
    out = []
    for line in text.split("\n"):
        m = TEXT_LINE.match(line)
        if m and "==" not in m.group(2) and "!=" not in m.group(2) and not m.group(2).lstrip().startswith("//"):
            line = m.group(1) + LITERAL.sub(literal, m.group(2))
        else:
            b = BARE_LITERAL.match(line)
            if b and b.group(2) in table and b.group(2) not in skip:
                line = f'{b.group(1)}"{table[b.group(2)]}"{b.group(3)}'
        out.append(line)
    return "\n".join(out)


def copy_icons(out: Path):
    shutil.copytree(HERE / "icons", out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--qrc", type=Path, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--dev", action="store_true", help="add the eval harness (never ship this)")
    a = ap.parse_args()
    shutil.rmtree(a.out, ignore_errors=True)
    base = {}
    for rel, change in edits(a.dev).items():
        if isinstance(change, str):
            base[rel] = change
            continue
        text = (a.qrc / rel).read_text(encoding="utf-8")
        for old, new in change:
            text = patch(text, old, new, rel)
        base[rel] = text
    rows = load_dict()
    skip = compared_strings(a.qrc)
    for lang in ("en", "zh-Hans", "zh-Hant"):
        table = {en: t[lang] for en, t in rows.items()} if lang != "en" else {}
        files = dict(base)
        if table:
            for f in a.qrc.rglob("*.qml"):
                rel = f.relative_to(a.qrc).as_posix()
                if "/Demo/" in rel:
                    continue
                source = files.get(rel) or f.read_text(encoding="utf-8", errors="strict")
                changed = translate(source, table, skip)
                if changed != source:
                    files[rel] = changed
        for rel, text in files.items():
            text = text.replace("@DICT@", json.dumps(table, ensure_ascii=False))
            target = a.out / lang / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        print(lang, len(files), "files")
    copy_icons(a.out / "icons")
    with tarfile.open(a.out.with_suffix(".tar.gz"), "w:gz") as t:
        for lang in ("en", "zh-Hans", "zh-Hant"):
            t.add(a.out / lang, arcname=f"packs/{lang}")
        t.add(a.out / "icons", arcname="icons")


if __name__ == "__main__":
    main()
