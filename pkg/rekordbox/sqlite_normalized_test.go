package rekordbox

import "testing"

func TestNormalizedColumnNames(t *testing.T) {
	for input, want := range map[string]string{
		"file_path":            "filepath",
		"File-Path":            "filepath",
		"release year":         "releaseyear",
		"analysisDataFilePath": "analysisdatafilepath",
		"Ärtist_Name":          "ärtistname",
	} {
		if got := normalized(input); got != want {
			t.Fatalf("normalized(%q) = %q, want %q", input, got, want)
		}
	}
}
