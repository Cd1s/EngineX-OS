package rekordbox

import (
	"encoding/xml"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// Standard Rekordbox XML structure for Pioneer DJ / AlphaTheta Rekordbox.
type XMLDatabase struct {
	XMLName    xml.Name      `xml:"DJ_PLAYLISTS"`
	Version    string        `xml:"Version,attr"`
	Product    XMLProduct    `xml:"PRODUCT"`
	Collection XMLCollection `xml:"COLLECTION"`
	Playlists  XMLPlaylists  `xml:"PLAYLISTS"`
}

type XMLProduct struct {
	Name    string `xml:"Name,attr"`
	Version string `xml:"Version,attr"`
	Company string `xml:"Company,attr"`
}

type XMLCollection struct {
	Entries int        `xml:"Entries,attr"`
	Tracks  []XMLTrack `xml:"TRACK"`
}

type XMLTrack struct {
	TrackID       string            `xml:"TrackID,attr"`
	Name          string            `xml:"Name,attr"`
	Artist        string            `xml:"Artist,attr"`
	Composer      string            `xml:"Composer,attr"`
	Album         string            `xml:"Album,attr"`
	Grouping      string            `xml:"Grouping,attr"`
	Genre         string            `xml:"Genre,attr"`
	Kind          string            `xml:"Kind,attr,omitempty"`
	Size          int64             `xml:"Size,attr,omitempty"`
	TotalTime     int               `xml:"TotalTime,attr,omitempty"`
	DiscNumber    int               `xml:"DiscNumber,attr"`
	TrackNumber   int               `xml:"TrackNumber,attr"`
	Year          int               `xml:"Year,attr,omitempty"`
	AverageBpm    string            `xml:"AverageBpm,attr,omitempty"`
	DateAdded     string            `xml:"DateAdded,attr,omitempty"`
	BitRate       int               `xml:"BitRate,attr,omitempty"`
	SampleRate    string            `xml:"SampleRate,attr,omitempty"`
	Comments      string            `xml:"Comments,attr"`
	PlayCount     int               `xml:"PlayCount,attr"`
	Rating        int               `xml:"Rating,attr"`
	Location      string            `xml:"Location,attr"`
	Remixer       string            `xml:"Remixer,attr"`
	Tonality      string            `xml:"Tonality,attr,omitempty"`
	Label         string            `xml:"Label,attr"`
	Mix           string            `xml:"Mix,attr"`
	Tempos        []XMLTempo        `xml:"TEMPO,omitempty"`
	PositionMarks []XMLPositionMark `xml:"POSITION_MARK,omitempty"`
}

type XMLTempo struct {
	Inizio  string `xml:"Inizio,attr"`
	Bpm     string `xml:"Bpm,attr"`
	Metro   string `xml:"Metro,attr"`
	Battito int    `xml:"Battito,attr"`
}

type XMLPositionMark struct {
	Name  string `xml:"Name,attr"`
	Type  string `xml:"Type,attr"`          // "0" = cue, "4" = loop
	Start string `xml:"Start,attr"`         // in seconds, e.g. "1.234"
	End   string `xml:"End,attr,omitempty"` // in seconds (only for Type="4")
	Num   string `xml:"Num,attr"`           // "0".."7" for hot cues/loops, "-1" for memory cues
	Red   string `xml:"Red,attr,omitempty"` // 0-255
	Green string `xml:"Green,attr,omitempty"`
	Blue  string `xml:"Blue,attr,omitempty"`
}

type XMLPlaylists struct {
	Root XMLNode `xml:"NODE"`
}

type XMLNode struct {
	Type    string         `xml:"Type,attr"` // "0" = folder, "1" = playlist
	Name    string         `xml:"Name,attr"`
	Count   int            `xml:"Count,attr,omitempty"`   // for folder
	KeyType string         `xml:"KeyType,attr,omitempty"` // "0" = TrackID
	Entries int            `xml:"Entries,attr,omitempty"` // for playlist
	Nodes   []XMLNode      `xml:"NODE,omitempty"`         // child folders or playlists
	Tracks  []XMLNodeTrack `xml:"TRACK,omitempty"`        // playlist tracks
}

type XMLNodeTrack struct {
	Key string `xml:"Key,attr"`
}

// NewXMLDatabase creates a Rekordbox XML structure with standard headers.
func NewXMLDatabase() *XMLDatabase {
	return &XMLDatabase{
		Version: "1.0.0",
		Product: XMLProduct{
			Name:    "rekordbox",
			Version: "6.6.2",
			Company: "AlphaTheta",
		},
		Collection: XMLCollection{
			Entries: 0,
			Tracks:  []XMLTrack{},
		},
		Playlists: XMLPlaylists{
			Root: XMLNode{
				Type:  "0",
				Name:  "ROOT",
				Count: 0,
				Nodes: []XMLNode{},
			},
		},
	}
}

// WriteTo writes the XMLDatabase with standard XML header and formatting, implementing io.WriterTo.
func (db *XMLDatabase) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	if _, err := io.WriteString(cw, xml.Header); err != nil {
		return cw.n, err
	}
	enc := xml.NewEncoder(cw)
	enc.Indent("", "  ")
	if err := enc.Encode(db); err != nil {
		return cw.n, err
	}
	if _, err := io.WriteString(cw, "\n"); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// EncodeXMLLocation converts a local or mounted file path to a Rekordbox XML file URI.
// E.g.: "file://localhost/Volumes/USB/Music/Track.mp3" or "file://localhost/E:/Music/Track.mp3"
func EncodeXMLLocation(filePath string) string {
	clean := filepath.ToSlash(filePath)
	clean = strings.TrimPrefix(clean, "/")
	parts := strings.Split(clean, "/")
	for i, p := range parts {
		if strings.HasSuffix(p, ":") {
			parts[i] = p
		} else {
			parts[i] = url.PathEscape(p)
		}
	}
	escaped := strings.Join(parts, "/")
	if !strings.HasPrefix(escaped, "/") {
		escaped = "/" + escaped
	}
	return "file://localhost" + escaped
}

// ParseHexRGB extracts decimal red, green, blue strings (0-255) from #RRGGBB.
func ParseHexRGB(hex string) (r, g, b string) {
	if len(hex) != 7 || hex[0] != '#' {
		return "", "", ""
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return "", "", ""
	}
	return strconv.Itoa(int((v >> 16) & 0xff)),
		strconv.Itoa(int((v >> 8) & 0xff)),
		strconv.Itoa(int(v & 0xff))
}
