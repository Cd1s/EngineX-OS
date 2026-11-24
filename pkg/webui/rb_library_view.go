package webui

// uiRbLibraryViews lists the allowed values for the rb_library_view setting.
// "folders" is the default when the key is absent from settings.conf.
var uiRbLibraryViews = []string{"folders", "merged", "device", "onelibrary"}

var mergedFolderTitles = map[string]string{
	"en":      "Merged Library",
	"zh-Hans": "合并曲库",
	"zh-Hant": "合併曲庫",
	"es":      "Biblioteca combinada",
}

// MergedFolderTitle returns the localized title for the merged rekordbox
// library folder in the playlist tree for the given deck language.
func MergedFolderTitle(lang string) string {
	if title, ok := mergedFolderTitles[lang]; ok {
		return title
	}
	return mergedFolderTitles["en"]
}
