package main

// Writing EDF+, the file format EEG labs exchange recordings in (EEGLAB, MNE,
// EDFbrowser and most amplifier software read it). An EDF file is a text
// header followed by "data records" of a fixed length, here 1 second. Each
// record holds every sensor's samples as 16-bit integers, and the header says
// which range of microvolts the integers stand for, per sensor. EDF+ adds one
// more signal, "EDF Annotations", that holds the markers as text.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	digitalMin = -32768
	digitalMax = 32767

	annotationLabel = "EDF Annotations"
	noDataText      = "NeuroHalo: no data" // where the file holds 0 µV because no samples arrived

	maxRecordSeconds = 60 // the longest data record tried by recordLayout
)

// annotation is one EDF+ annotation: something that happened onset seconds
// into the file and lasted duration seconds (0 for a single moment).
// Built by: merger.add (markers and gaps) and writeEDF (padding).
// Used by: writeEDF, which places each one in the data record it falls in.
type annotation struct {
	onset    float64
	duration float64
	text     string
}

// edfFile is everything writeEDF needs besides the samples.
// Built by: recorder.finalize, from the session row and the merger.
// Used by: writeEDF.
type edfFile struct {
	sessionID   string
	start       time.Time
	rate        float64
	names       []string
	samples     int64        // per sensor, before the last record is padded
	min, max    []float64    // per sensor, the smallest and largest value in µV
	annotations []annotation // any order
}

// recordLayout picks the length of one data record: the shortest whole
// number of seconds (1 to 60) that holds a whole number of samples. It
// returns the length and the samples per sensor in one record.
func recordLayout(rate float64) (seconds, perRecord int, err error) {
	for d := 1; d <= maxRecordSeconds; d++ {
		n := rate * float64(d)
		if r := math.Round(n); r >= 1 && math.Abs(n-r) < 1e-6 {
			return d, int(r), nil
		}
	}
	return 0, 0, permanent(fmt.Errorf("a sample rate of %g Hz does not fit a whole number of samples into %d seconds", rate, maxRecordSeconds))
}

// physicalRange turns a sensor's smallest and largest value into the two
// 8-character header fields, rounded outwards so every value fits. It returns
// the fields and the numbers they spell, which are what readers will use.
// A sensor with one constant value gets a range of 2 µV around it.
func physicalRange(lo, hi float64) (loText, hiText string, loValue, hiValue float64) {
	if !(hi > lo) {
		lo, hi = lo-1, hi+1
	}
	const limit = 9999999 // "-9999999" is the widest number 8 characters hold
	lo, hi = max(lo, -limit), min(hi, limit)
	for decimals := 3; decimals >= 0; decimals-- {
		scale := math.Pow(10, float64(decimals))
		l := math.Floor(lo*scale) / scale
		h := math.Ceil(hi*scale) / scale
		loText = strconv.FormatFloat(l, 'f', decimals, 64)
		hiText = strconv.FormatFloat(h, 'f', decimals, 64)
		if len(loText) <= 8 && len(hiText) <= 8 {
			loValue, _ = strconv.ParseFloat(loText, 64)
			hiValue, _ = strconv.ParseFloat(hiText, 64)
			return
		}
	}
	return "-9999999", "9999999", -limit, limit // not reached: the clamp above keeps decimals=0 in 8 characters
}

// seconds formats a time for an annotation: plain decimal, at most
// microsecond precision, no trailing zeros ("12.5", "0", "3.000625").
func seconds(v float64) string {
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ascii keeps the header readable to every reader: characters outside
// printable ASCII become '_'.
func ascii(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '_'
		}
		return r
	}, s)
}

// tal encodes one annotation as an EDF+ "time-stamped annotation list":
// +onset[0x15 duration]0x14 text 0x14 0x00. The text may be UTF-8; the three
// bytes that have a meaning in a TAL are replaced by spaces.
func tal(a annotation) []byte {
	text := strings.Map(func(r rune) rune {
		if r == 0 || r == 0x14 || r == 0x15 {
			return ' '
		}
		return r
	}, a.text)
	var b []byte
	b = append(b, '+')
	b = append(b, seconds(a.onset)...)
	if a.duration > 0 {
		b = append(b, 0x15)
		b = append(b, seconds(a.duration)...)
	}
	b = append(b, 0x14)
	b = append(b, text...)
	return append(b, 0x14, 0)
}

// headerWriter writes fixed-width header fields: padded with spaces, or cut.
// Used by: writeEDF.
type headerWriter struct{ b []byte }

// field appends s as exactly width characters.
func (h *headerWriter) field(s string, width int) {
	s = ascii(s)
	if len(s) > width {
		s = s[:width]
	}
	h.b = append(h.b, s...)
	h.b = append(h.b, strings.Repeat(" ", width-len(s))...)
}

// writeEDF writes an EDF+ file ("EDF+C", continuous) to w. rows holds the
// samples as float32 little-endian, one row of every sensor per sample, in
// time order: f.samples rows. The last data record is filled up with 0 µV,
// marked by a "no data" annotation. Every value is converted with its
// sensor's own range, so the 16 bits cover exactly what the sensor recorded.
func writeEDF(w io.Writer, f *edfFile, rows io.Reader) error {
	channels := len(f.names)
	if channels == 0 || f.samples <= 0 {
		return permanent(errors.New("no samples to write"))
	}
	recordSeconds, perRecord, err := recordLayout(f.rate)
	if err != nil {
		return err
	}
	records := (f.samples + int64(perRecord) - 1) / int64(perRecord)
	notes := append([]annotation(nil), f.annotations...)
	lo, hi := append([]float64(nil), f.min...), append([]float64(nil), f.max...)
	if pad := records*int64(perRecord) - f.samples; pad > 0 {
		notes = append(notes, annotation{onset: float64(f.samples) / f.rate, duration: float64(pad) / f.rate, text: noDataText})
		for c := range lo { // the padding is 0 µV, so 0 must be inside every range
			lo[c], hi[c] = min(lo[c], 0), max(hi[c], 0)
		}
	}

	// The annotations of each data record: the record's own start time first
	// (EDF+ requires it), then the markers that fall in it.
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].onset < notes[j].onset })
	perRecordNotes := make(map[int64][]byte)
	for _, a := range notes {
		r := min(max(int64(a.onset/float64(recordSeconds)), 0), records-1)
		perRecordNotes[r] = append(perRecordNotes[r], tal(a)...)
	}
	annBytes := 0
	for r := int64(0); r < records; r++ {
		keep := len(tal(annotation{onset: float64(r * int64(recordSeconds))})) + len(perRecordNotes[r])
		annBytes = max(annBytes, keep)
	}
	annSamples := (annBytes + 1) / 2

	// The header: 256 bytes, then 256 bytes per signal, field by field.
	ns := channels + 1
	var h headerWriter
	start := f.start.UTC()
	h.field("0", 8)
	h.field("X X X X", 80) // patient: code, sex, birth date, name, all unknown
	h.field(fmt.Sprintf("Startdate %02d-%s-%04d %s X NeuroHalo",
		start.Day(), strings.ToUpper(start.Format("Jan")), start.Year(), strings.ReplaceAll(f.sessionID, " ", "_")), 80)
	h.field(start.Format("02.01.06"), 8)
	h.field(start.Format("15.04.05"), 8)
	h.field(strconv.Itoa(256*(ns+1)), 8)
	h.field("EDF+C", 44)
	h.field(strconv.FormatInt(records, 10), 8)
	h.field(strconv.Itoa(recordSeconds), 8)
	h.field(strconv.Itoa(ns), 4)

	loText, hiText := make([]string, channels), make([]string, channels)
	offset, gain := make([]float64, channels), make([]float64, channels)
	for c := range channels {
		var l, u float64
		loText[c], hiText[c], l, u = physicalRange(lo[c], hi[c])
		gain[c] = (u - l) / (digitalMax - digitalMin)
		offset[c] = l
	}
	each := func(width int, value func(c int) string, annotationValue string) {
		for c := range channels {
			h.field(value(c), width)
		}
		h.field(annotationValue, width)
	}
	each(16, func(c int) string { return f.names[c] }, annotationLabel)
	each(80, func(int) string { return "" }, "")           // transducer
	each(8, func(int) string { return "uV" }, "")          // physical unit
	each(8, func(c int) string { return loText[c] }, "-1") // physical minimum
	each(8, func(c int) string { return hiText[c] }, "1")  // physical maximum
	each(8, func(int) string { return strconv.Itoa(digitalMin) }, strconv.Itoa(digitalMin))
	each(8, func(int) string { return strconv.Itoa(digitalMax) }, strconv.Itoa(digitalMax))
	each(80, func(int) string { return "" }, "") // prefiltering
	each(8, func(int) string { return strconv.Itoa(perRecord) }, strconv.Itoa(annSamples))
	each(32, func(int) string { return "" }, "") // reserved

	out := bufio.NewWriterSize(w, 1<<16)
	if _, err := out.Write(h.b); err != nil {
		return err
	}

	// The data records. Each one is read as rows, then written sensor by
	// sensor, which is the order EDF keeps them in.
	in := make([]byte, perRecord*channels*4)
	digital := make([]byte, perRecord*2)
	ann := make([]byte, annSamples*2)
	for r := int64(0); r < records; r++ {
		n := int(min(int64(perRecord), f.samples-r*int64(perRecord)))
		clear(in)
		if _, err := io.ReadFull(rows, in[:n*channels*4]); err != nil {
			return fmt.Errorf("reading samples of record %d: %w", r, err)
		}
		for c := range channels {
			for i := range perRecord {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(in[(i*channels+c)*4:])))
				d := math.Round((v-offset[c])/gain[c]) + digitalMin
				binary.LittleEndian.PutUint16(digital[i*2:], uint16(int16(min(max(d, digitalMin), digitalMax))))
			}
			if _, err := out.Write(digital); err != nil {
				return err
			}
		}
		// The record's start time comes first, as a TAL with no text
		// ("+12" 0x14 0x14 0x00), then the record's annotations.
		clear(ann)
		n = copy(ann, tal(annotation{onset: float64(r * int64(recordSeconds))}))
		copy(ann[n:], perRecordNotes[r])
		if _, err := out.Write(ann); err != nil {
			return err
		}
	}
	return out.Flush()
}
