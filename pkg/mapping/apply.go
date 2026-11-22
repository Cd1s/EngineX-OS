package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status is what the last start did with the DJ's mappings, for the console.
type Status struct {
	State    string `json:"state"` // applied | rejected | none
	Board    string `json:"board"`
	Mappings int    `json:"mappings,omitempty"`
	Code     string `json:"code,omitempty"`   // the Reason of a rejection the console can translate
	Reason   string `json:"reason,omitempty"` // the message, for the log and for what has no code
	Time     string `json:"time"`
}

// StatusPath is the status file of a board next to the patched assignment files.
func StatusPath(dir, board string) string { return filepath.Join(dir, "mapping-"+board+".json") }

// WriteStatus replaces the status file.
func WriteStatus(path string, st Status) error {
	st.Time = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// ReadStatus reads it; a missing file is the zero Status.
func ReadStatus(path string) (Status, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".map-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// Apply is what the guard runs at Engine start for one board's assignment file. It adds the
// board's mappings from file to the assignment file in, which is stock's or stock's with the
// 4-deck role patch, and writes the result to out. It returns whether out was written. When
// there is nothing to add, or anything fails, out is not written and the caller binds nothing:
// stock stays. The outcome goes to the status file for the console.
func Apply(board, in, out, file, statusPath string) (bool, error) {
	record := func(st Status) {
		st.Board = board
		if err := WriteStatus(statusPath, st); err != nil {
			fmt.Fprintln(os.Stderr, "mapping: status:", err)
		}
	}
	reject := func(err error) (bool, error) {
		st := Status{State: "rejected", Reason: err.Error()}
		var e *Error
		if errors.As(err, &e) {
			st.Code = e.Code
		}
		record(st)
		return false, err
	}
	list, err := Load(file)
	if err != nil {
		return reject(err)
	}
	mine := ForBoard(list, board)
	if len(mine) == 0 {
		record(Status{State: "none"})
		return false, nil
	}
	stock, err := os.ReadFile(in)
	if err != nil {
		return reject(err)
	}
	patched, err := Generate(board, stock, mine)
	if err != nil {
		return reject(err)
	}
	if err := writeAtomic(out, patched); err != nil {
		return reject(err)
	}
	record(Status{State: "applied", Mappings: len(mine)})
	return true, nil
}
