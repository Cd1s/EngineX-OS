package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type recordedSet struct {
	ID        int64  `json:"id"`
	Drive     string `json:"drive"`
	Title     string `json:"title"`
	StartTime string `json:"startTime"`
	Timezone  string `json:"timezone"`
}

func openHistory(d drive) (*sql.DB, error) {
	d.db = filepath.Join(filepath.Dir(d.db), "hm.db")
	st, err := os.Stat(d.db)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("history is not a database file")
	}
	return openDrive(d) // mode=ro, query_only; Engine remains the only writer.
}

func historyTime(value string) string {
	if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
	}
	return value
}

func driveRecordedSets(ctx context.Context, d drive) ([]recordedSet, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := openHistory(d)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(title,''),
		CASE WHEN timezone IS NULL THEN '' ELSE COALESCE(startTime,'') END,
		COALESCE(timezone,'') FROM Historylist WHERE COALESCE(isDeleted,0)=0
		ORDER BY startTime DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sets []recordedSet
	for rows.Next() {
		set := recordedSet{Drive: d.name}
		if err := rows.Scan(&set.ID, &set.Title, &set.StartTime, &set.Timezone); err != nil {
			return nil, err
		}
		set.StartTime = historyTime(set.StartTime)
		sets = append(sets, set)
	}
	return sets, rows.Err()
}

func (s *Server) tracklists(w http.ResponseWriter, r *http.Request) {
	sets := []recordedSet{}
	unavailable := []string{}
	for _, d := range s.drives() {
		found, err := driveRecordedSets(r.Context(), d)
		if err != nil {
			unavailable = append(unavailable, d.name)
			continue
		}
		sets = append(sets, found...)
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, struct {
		Sets        []recordedSet `json:"sets"`
		Unavailable []string      `json:"unavailable"`
	}{sets, unavailable})
}

// CSV metadata comes from user libraries. Quoting alone does not prevent a
// spreadsheet from evaluating a title/artist as a formula.
func tracklistCSVCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if value != "" && (strings.ContainsAny(value[:1], "\t\r\n") || trimmed != "" && strings.ContainsAny(trimmed[:1], "=+-@")) {
		return "'" + value
	}
	return value
}

func (s *Server) exportTracklist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, format := q.Get("drive"), q.Get("format")
	id, err := strconv.ParseInt(q.Get("id"), 10, 64)
	if err != nil || id <= 0 || (format != "csv" && format != "txt") {
		fail(w, 400, errors.New("a recorded set and csv or txt format are required"))
		return
	}
	if _, err := validateDrive(s.mediaRoot(), name); err != nil {
		fail(w, 400, err)
		return
	}
	var selected drive
	for _, d := range s.drives() {
		if d.name == name {
			selected = d
			break
		}
	}
	if selected.db == "" {
		http.NotFound(w, r)
		return
	}
	db, err := openHistory(selected)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 500, errors.New("could not read recorded set"))
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	body, err := recordedSetExport(ctx, db, id, format)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, 500, errors.New("could not read recorded set"))
		return
	}
	contentType := "text/plain; charset=utf-8"
	if format == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="enginex-set-%d.%s"`, id, format))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}

func recordedSetExport(ctx context.Context, db *sql.DB, id int64, format string) ([]byte, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var zone sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT timezone FROM Historylist WHERE id=? AND COALESCE(isDeleted,0)=0`, id).Scan(&zone); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(e.startTime,''), COALESCE(t.artist,''), COALESCE(t.title,'')
		FROM HistorylistEntity e LEFT JOIN Track t ON t.id=e.trackId
		WHERE e.listId=? ORDER BY e.startTime, e.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var body bytes.Buffer
	csvOut := csv.NewWriter(&body)
	oneLine := strings.NewReplacer("\r", " ", "\n", " ", "\t", " ")
	if format == "csv" {
		body.WriteString("\ufeff") // Let spreadsheet apps recognize UTF-8 metadata.
		csvOut.Write([]string{"played_at", "timezone", "artist", "title"})
	}
	for rows.Next() {
		var played, artist, title string
		if err := rows.Scan(&played, &artist, &title); err != nil {
			return nil, err
		}
		// Stock treats history timestamps without a timezone as unknown (for
		// example sets made before the clock was set); do not invent a date.
		if zone.Valid {
			played = historyTime(played)
		} else {
			played = ""
		}
		if format == "csv" {
			csvOut.Write([]string{tracklistCSVCell(played), tracklistCSVCell(zone.String), tracklistCSVCell(artist), tracklistCSVCell(title)})
		} else {
			// One play per line, even if the metadata contains line breaks.
			fmt.Fprintf(&body, "%s\t%s\t%s - %s\n", oneLine.Replace(played), oneLine.Replace(zone.String), oneLine.Replace(artist), oneLine.Replace(title))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	csvOut.Flush()
	return body.Bytes(), csvOut.Error()
}
