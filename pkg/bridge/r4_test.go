package bridge

import "testing"

func TestR4PathFirstUsesReadOrderAndUnknownUnion(t *testing.T) {
	old := "v4 PIONEER/rekordbox/exportLibrary.db 1 2 | PIONEER/rekordbox/export.pdb 3 4"
	if !pathFirstFor(old, "", []string{"PIONEER/rekordbox/export.pdb"}, old) {
		t.Fatal("a released multi-library stamp must use path-first matching")
	}
	mark := old + "\nPIONEER/rekordbox/exportLibrary.db\n"
	if !pathFirstFor(old, mark, []string{"PIONEER/rekordbox/export.pdb"}, old) {
		t.Fatal("a readable-source change must use path-first matching")
	}
	if pathFirstFor(old, mark, []string{"PIONEER/rekordbox/exportLibrary.db"}, old) {
		t.Fatal("unchanged readable source must keep source-ID precedence")
	}
}

func TestR4PathFirstDetectsStampOrderChange(t *testing.T) {
	old := "v4 PIONEER/rekordbox/exportLibrary.db 1 2 | PIONEER/rekordbox/export.pdb 3 4"
	mark := old + "\nPIONEER/rekordbox/exportLibrary.db\nPIONEER/rekordbox/export.pdb\n"
	cur := "v4 PIONEER/rekordbox/export.pdb 3 5 | PIONEER/rekordbox/exportLibrary.db 1 2"
	if !pathFirstFor(old, mark, []string{"PIONEER/rekordbox/export.pdb", "PIONEER/rekordbox/exportLibrary.db"}, cur) {
		t.Fatal("stamp-order change must use path-first matching")
	}
}

func TestR4PathFirstUnknownUnionUsesPathMatching(t *testing.T) {
	old := "v4 PIONEER/rekordbox/exportLibrary.db 1 2 | PIONEER/rekordbox/export.pdb 3 4"
	if !pathFirstFor(old, "", []string{"PIONEER/rekordbox/exportLibrary.db"}, old) {
		t.Fatal("a released multi-library stamp with an unknown source must use path-first matching")
	}
}

func TestR4PathFirstDetectsStampPrimaryChangeWithSameReadOrder(t *testing.T) {
	old := "v4 PIONEER/rekordbox/exportLibrary.db 1 2 | PIONEER/rekordbox/export.pdb 3 4"
	mark := old + "\nPIONEER/rekordbox/export.pdb\nPIONEER/rekordbox/exportLibrary.db\n"
	current := "v4 PIONEER/rekordbox/export.pdb 3 5 | PIONEER/rekordbox/exportLibrary.db 1 2"
	if !pathFirstFor(old, mark, []string{"PIONEER/rekordbox/export.pdb"}, current) {
		t.Fatal("a changed stamp primary must use path-first matching even when the read order is unchanged")
	}
}
