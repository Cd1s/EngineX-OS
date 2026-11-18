# Settings Polish

Community Mod by JaviLabarumDJ (issue #219), MIT licensed, declarative QML only: no script, no helper, no network.

Cleaner Preferences and Layout screens:

- **ListEntry.qml**: list options become a segmented control (rounded pills, the chosen one tinted and outlined in the active colour). It keeps EngineX OS's own `ListEntry.qml` changes (`isSelected(value)`, translated labels through `root.scT`, selection by `modelData`).
- **PrefsHeadline.qml**: section headlines line up with the rows, with a little air above them and a quieter grey.
- **PrefsEntryButton.qml**: entry buttons are rounded and lighter while pressed.

Visual only: selection and values behave exactly as in EngineX OS. The Mod replaces three stock files while enabled and the stock files come back when it is disabled or removed. Built for EngineX OS 5.3.0.4; since it overrides `ListEntry.qml`, re-check it when a release changes that file.
