// Package meta reads the metadata embedded in files (EXIF, XMP, PDF, Office)
// and writes a card's metadata back as XMP, through exiftool when present.
package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Available reports whether exiftool can be run.
func Available() bool {
	_, err := exec.LookPath("exiftool")
	return err == nil
}

// Embedded is what artefact keeps of a file's own metadata.
type Embedded struct {
	Date     string    `yaml:"date,omitempty" json:"date,omitempty"` // YYYY-MM-DD: taken, created
	Title    string    `yaml:"title,omitempty" json:"title,omitempty"`
	Author   string    `yaml:"author,omitempty" json:"author,omitempty"`
	Subject  string    `yaml:"subject,omitempty" json:"subject,omitempty"`
	Keywords []string  `yaml:"keywords,omitempty" json:"keywords,omitempty"`
	Camera   string    `yaml:"camera,omitempty" json:"camera,omitempty"`
	GPS      []float64 `yaml:"gps,omitempty,flow" json:"gps,omitempty"` // latitude, longitude
	Pages    int       `yaml:"pages,omitempty" json:"pages,omitempty"`
	Width    int       `yaml:"width,omitempty" json:"width,omitempty"`
	Height   int       `yaml:"height,omitempty" json:"height,omitempty"`
	Duration float64   `yaml:"duration,omitempty" json:"duration,omitempty"` // seconds
	Producer string    `yaml:"producer,omitempty" json:"producer,omitempty"` // software or device that made the file
}

func (e Embedded) Empty() bool {
	return e.Date == "" && e.Title == "" && e.Author == "" && e.Subject == "" && len(e.Keywords) == 0 &&
		e.Camera == "" && len(e.GPS) == 0 && e.Pages == 0 && e.Width == 0 && e.Duration == 0 && e.Producer == ""
}

var dateRe = regexp.MustCompile(`^(\d{4})[:-](\d{2})[:-](\d{2})`)

// Read extracts a file's metadata. ext gives the format when the file's own
// name has none (a payload is stored as payload.<ext>).
func Read(path string) (Embedded, error) {
	var e Embedded
	if !Available() {
		return e, fmt.Errorf("exiftool is not installed (brew install exiftool)")
	}
	out, err := exec.Command("exiftool", "-j", "-n", "-G1", "-q", "-q",
		"-DateTimeOriginal", "-CreateDate", "-ContentCreateDate", "-MediaCreateDate", "-XMP-dc:Date",
		"-XMP-dc:Title", "-PDF:Title", "-XMP-dc:Creator", "-PDF:Author", "-EXIF:Artist", "-Author", "-LastModifiedBy",
		"-XMP-dc:Subject", "-PDF:Subject", "-Keywords", "-PDF:Keywords",
		"-Make", "-Model", "-GPSLatitude", "-GPSLongitude",
		"-PageCount", "-Pages", "-ImageWidth", "-ImageHeight", "-Duration",
		"-PDF:Creator", "-Producer", "-Software", "-Application",
		path).Output()
	if err != nil && len(out) == 0 {
		return e, fmt.Errorf("exiftool: %w", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) == 0 {
		return e, nil
	}
	// Keys come as Group:Tag; keep them, and the bare tag for the first group seen.
	m := map[string]any{}
	for k, v := range rows[0] {
		m[k] = v
		if _, tag, ok := strings.Cut(k, ":"); ok {
			if _, seen := m[tag]; !seen {
				m[tag] = v
			}
		}
	}
	// XMP dc:subject holds keywords; a PDF's Subject is a description.
	if v, ok := m["XMP-dc:Subject"]; ok {
		m["Keywords"] = v
		delete(m, "Subject")
		if p, ok := m["PDF:Subject"]; ok {
			m["Subject"] = p
		}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			switch v := m[k].(type) {
			case string:
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			case float64:
				return fmt.Sprint(v)
			case []any:
				var parts []string
				for _, x := range v {
					parts = append(parts, fmt.Sprint(x))
				}
				if len(parts) > 0 {
					return strings.Join(parts, ", ")
				}
			}
		}
		return ""
	}
	num := func(keys ...string) float64 {
		for _, k := range keys {
			if v, ok := m[k].(float64); ok && v > 0 {
				return v
			}
		}
		return 0
	}
	for _, k := range []string{"Date", "DateTimeOriginal", "ContentCreateDate", "MediaCreateDate", "CreateDate"} {
		if s, ok := m[k].(string); ok {
			if d := dateRe.FindStringSubmatch(s); d != nil && d[1] != "0000" {
				e.Date = d[1] + "-" + d[2] + "-" + d[3]
				break
			}
		}
	}
	e.Title = str("Title")
	e.Author = str("Creator", "Author", "Artist")
	if e.Author == str("Creator") && looksLikeSoftware(e.Author) {
		e.Author = str("Author", "Artist")
	}
	e.Subject = str("Subject")
	switch v := m["Keywords"].(type) {
	case string:
		for _, k := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' }) {
			if k = strings.TrimSpace(k); k != "" {
				e.Keywords = append(e.Keywords, k)
			}
		}
	case []any:
		for _, x := range v {
			e.Keywords = append(e.Keywords, fmt.Sprint(x))
		}
	}
	make_, model := str("Make"), str("Model")
	if model != "" {
		e.Camera = strings.TrimSpace(strings.TrimPrefix(model, make_+" "))
		if make_ != "" && !strings.HasPrefix(model, make_) {
			e.Camera = make_ + " " + model
		}
	}
	if lat, lon := num("GPSLatitude"), m["GPSLongitude"]; lat != 0 {
		if l, ok := lon.(float64); ok {
			e.GPS = []float64{round6(lat), round6(l)}
		}
	}
	e.Pages = int(num("PageCount", "Pages"))
	e.Width, e.Height = int(num("ImageWidth")), int(num("ImageHeight"))
	e.Duration = num("Duration")
	e.Producer = str("Software", "Producer", "Application")
	if c := str("Creator"); e.Producer == "" && looksLikeSoftware(c) {
		e.Producer = c
	}
	return e, nil
}

func round6(f float64) float64 { return float64(int64(f*1e6)) / 1e6 }

// looksLikeSoftware tells a PDF Creator that names a program or a scanner
// from one that names a person.
func looksLikeSoftware(s string) bool {
	l := strings.ToLower(s)
	for _, w := range []string{"pdf", "word", "acrobat", "canon", "xerox", "ricoh", "konica", "scan", "microsoft", "libreoffice", "quartz", "pages", "latex", "chrome", "printer", "adobe", "®", "™", "version", " v1", " v2"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// Writable reports whether exiftool can write XMP into files of this extension.
func Writable(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".tif", ".tiff", ".heic", ".heif", ".webp", ".pdf", ".dng", ".mp4", ".mov", ".m4a", ".gif", ".avif":
		return true
	}
	return false
}

// Fields are the card values written into a file as XMP.
type Fields struct {
	Title       string
	Date        string // YYYY-MM-DD
	Creator     string
	Description string
	Keywords    []string
}

// Write sets the XMP Dublin Core fields of the file at path in place.
func Write(path string, f Fields) error {
	if !Available() {
		return fmt.Errorf("exiftool is not installed (brew install exiftool)")
	}
	if !Writable(filepath.Ext(path)) {
		return fmt.Errorf("artefact writes metadata into images, PDF and videos only, not %s files", filepath.Ext(path))
	}
	args := []string{"-overwrite_original", "-q", "-m"}
	if f.Title != "" {
		args = append(args, "-XMP-dc:Title="+f.Title)
	}
	if f.Date != "" {
		args = append(args, "-XMP-dc:Date="+strings.ReplaceAll(f.Date, "-", ":"))
	}
	if f.Creator != "" {
		args = append(args, "-XMP-dc:Creator="+f.Creator)
	}
	if f.Description != "" {
		args = append(args, "-XMP-dc:Description="+f.Description)
	}
	if len(f.Keywords) > 0 {
		args = append(args, "-XMP-dc:Subject=")
		for _, k := range f.Keywords {
			args = append(args, "-XMP-dc:Subject+="+k)
		}
	}
	if len(args) == 3 {
		return nil
	}
	args = append(args, path)
	var stderr bytes.Buffer
	cmd := exec.Command("exiftool", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("exiftool: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
