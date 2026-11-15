package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/neurohalo/neurohalo/internal/recording"

	neurov1 "github.com/neurohalo/neurohalo/gen/neuro/v1"
)

// batch makes a SampleBatch of n samples on 2 sensors starting at sample
// first; sensor 0 holds the sample index, sensor 1 its negative.
func batch(seq, first uint64, n int, markers ...*neurov1.Marker) *neurov1.SampleBatch {
	b := &neurov1.SampleBatch{SessionId: "s", Seq: seq, FirstSampleIndex: first, NumSamples: uint32(n), Markers: markers}
	for i := range n {
		v := float32(first) + float32(i)
		b.Values = append(b.Values, v, -v)
	}
	return b
}

// rowsOf decodes the merger's output back into rows of 2 sensors.
func rowsOf(t *testing.T, raw []byte) [][2]float32 {
	t.Helper()
	var rows [][2]float32
	for i := 0; i+8 <= len(raw); i += 8 {
		rows = append(rows, [2]float32{
			math.Float32frombits(binary.LittleEndian.Uint32(raw[i:])),
			math.Float32frombits(binary.LittleEndian.Uint32(raw[i+4:])),
		})
	}
	return rows
}

func TestMerger(t *testing.T) {
	var out bytes.Buffer
	m := newMerger(4, 2, &out)
	steps := []*neurov1.SampleBatch{
		batch(10, 100, 4), // samples 100-103; the session starts at 100
		batch(11, 104, 4, &neurov1.Marker{SampleIndex: 105, Label: "T1", DurationSeconds: 0.5}),
		batch(11, 104, 4, &neurov1.Marker{SampleIndex: 105, Label: "T1"}), // resent: dropped
		batch(9, 96, 4), // older seq: dropped
		batch(12, 106, 4, &neurov1.Marker{SampleIndex: 106, Label: "late"}), // overlaps 106-107; only 108-109 are new
		batch(14, 114, 2), // samples 110-113 never arrived
		{SessionId: "s", Seq: 15, FirstSampleIndex: 116, NumSamples: 2, Values: []float32{1}}, // wrong value count
	}
	for _, b := range steps {
		if err := m.add(b); err != nil {
			t.Fatal(err)
		}
	}
	if m.batches != 4 || m.duplicates != 2 || m.malformed != 1 {
		t.Errorf("batches=%d duplicates=%d malformed=%d, want 4, 2, 1", m.batches, m.duplicates, m.malformed)
	}
	rows := rowsOf(t, out.Bytes())
	if len(rows) != 16 || m.samples != 16 {
		t.Fatalf("%d rows (samples=%d), want 16 (100 to 115)", len(rows), m.samples)
	}
	for i, r := range rows {
		want := float32(100 + i)
		if i >= 10 && i < 14 {
			want = 0 // the gap
		}
		if r[0] != want || r[1] != -want {
			t.Errorf("row %d = %v, want [%g %g]", i, r, want, -want)
		}
	}
	wantNotes := []annotation{
		{onset: 1.25, duration: 0.5, text: "T1"},
		{onset: 2.5, duration: 1, text: noDataText},
	}
	if len(m.annotations) != len(wantNotes) {
		t.Fatalf("annotations = %+v, want %+v", m.annotations, wantNotes)
	}
	for i, a := range wantNotes {
		if m.annotations[i] != a {
			t.Errorf("annotation %d = %+v, want %+v", i, m.annotations[i], a)
		}
	}
	if m.min[0] != 0 || m.max[0] != 115 || m.min[1] != -115 || m.max[1] != 0 {
		t.Errorf("ranges = %v..%v, want [0 -115]..[115 0]", m.min, m.max)
	}
}

func TestChunkRoundTrip(t *testing.T) {
	var c chunk
	sent := []*neurov1.SampleBatch{batch(3, 30, 10), batch(4, 40, 10, &neurov1.Marker{SampleIndex: 41, Label: "x"})}
	for _, b := range sent {
		value, err := proto.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		c.add(b.Seq, value)
	}
	if c.firstSeq != 3 || c.lastSeq != 4 {
		t.Errorf("seq range %d-%d, want 3-4", c.firstSeq, c.lastSeq)
	}
	var got []*neurov1.SampleBatch
	err := readChunk(bufio.NewReader(&c.buf), func(b *neurov1.SampleBatch) error {
		got = append(got, b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(sent) {
		t.Fatalf("read %d batches, want %d", len(got), len(sent))
	}
	for i := range sent {
		if !proto.Equal(got[i], sent[i]) {
			t.Errorf("batch %d changed on the way", i)
		}
	}
	if err := readChunk(bufio.NewReader(bytes.NewReader([]byte{10, 1, 2})), func(*neurov1.SampleBatch) error { return nil }); err == nil {
		t.Error("a cut-off chunk was read without an error")
	}
}

func TestChunkKeys(t *testing.T) {
	c := &chunk{firstSeq: 7, lastSeq: 12}
	if got := chunkKey("a/b", c); got != "spool/a%2Fb/00000000000000000007-00000000000000000012.bin" {
		t.Errorf("chunkKey = %q", got)
	}
	if strings.HasPrefix(recording.SpoolDir("a/b"), recording.SpoolDir("a")) {
		t.Error("session a's folder contains session a/b's")
	}
}

func TestRecordLayout(t *testing.T) {
	cases := []struct {
		rate            float64
		seconds, perRec int
		ok              bool
	}{
		{160, 1, 160, true},
		{256, 1, 256, true},
		{128.5, 2, 257, true},
		{math.Pi, 0, 0, false},
	}
	for _, c := range cases {
		s, n, err := recordLayout(c.rate)
		if (err == nil) != c.ok || s != c.seconds || n != c.perRec {
			t.Errorf("recordLayout(%g) = %d, %d, %v; want %d, %d, ok=%v", c.rate, s, n, err, c.seconds, c.perRec, c.ok)
		}
	}
}

func TestPhysicalRange(t *testing.T) {
	cases := [][2]float64{{-150.25, 98.004}, {-3276.8, 3276.7}, {-123456.789, 5}, {12, 12}, {0, 0}, {-1e9, 1e9}}
	for _, c := range cases {
		loText, hiText, lo, hi := physicalRange(c[0], c[1])
		if len(loText) > 8 || len(hiText) > 8 {
			t.Errorf("%v: fields %q %q are longer than 8 characters", c, loText, hiText)
		}
		if !(hi > lo) {
			t.Errorf("%v: range %g..%g is empty", c, lo, hi)
		}
		if math.Abs(c[0]) < 9999999 && (lo > c[0] || hi < c[1]) {
			t.Errorf("%v: range %g..%g does not hold the values", c, lo, hi)
		}
	}
}

// parsedEDF is what readEDF found in a file. Used by: TestWriteEDF.
type parsedEDF struct {
	header  []byte
	fields  map[string][]string // per-signal header fields, by name
	signals [][]float64         // per sensor, physical values
	tals    []string            // every TAL, the 0x14/0x15 bytes as | and ~
}

// readEDF parses a file the way a reader would: the header, then each data
// record, converting digital values with each signal's ranges.
func readEDF(t *testing.T, b []byte) parsedEDF {
	t.Helper()
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			t.Fatalf("header number %q: %v", s, err)
		}
		return v
	}
	ns := int(num(string(b[252:256])))
	records := int(num(string(b[236:244])))
	if int(num(string(b[184:192]))) != 256*(ns+1) {
		t.Fatalf("header size field does not match %d signals", ns)
	}
	p := parsedEDF{header: b[:256], fields: map[string][]string{}}
	pos := 256
	for _, f := range []struct {
		name  string
		width int
	}{{"label", 16}, {"transducer", 80}, {"unit", 8}, {"pmin", 8}, {"pmax", 8}, {"dmin", 8}, {"dmax", 8}, {"prefilter", 80}, {"samples", 8}, {"reserved", 32}} {
		for range ns {
			p.fields[f.name] = append(p.fields[f.name], strings.TrimSpace(string(b[pos:pos+f.width])))
			pos += f.width
		}
	}
	p.signals = make([][]float64, ns-1)
	for r := 0; r < records; r++ {
		for s := range ns {
			n := int(num(p.fields["samples"][s]))
			if p.fields["label"][s] == annotationLabel {
				for _, one := range bytes.Split(bytes.TrimRight(b[pos:pos+2*n], "\x00"), []byte{0}) {
					if len(one) > 0 {
						p.tals = append(p.tals, strings.NewReplacer("\x14", "|", "\x15", "~").Replace(string(one)))
					}
				}
			} else {
				pmin, pmax := num(p.fields["pmin"][s]), num(p.fields["pmax"][s])
				dmin, dmax := num(p.fields["dmin"][s]), num(p.fields["dmax"][s])
				gain := (pmax - pmin) / (dmax - dmin)
				for i := range n {
					d := float64(int16(binary.LittleEndian.Uint16(b[pos+2*i:])))
					p.signals[s] = append(p.signals[s], (d-dmin)*gain+pmin)
				}
			}
			pos += 2 * n
		}
	}
	if pos != len(b) {
		t.Fatalf("file is %d bytes, header and records account for %d", len(b), pos)
	}
	return p
}

func TestWriteEDF(t *testing.T) {
	// 2.5 seconds at 4 Hz on two sensors, so the last record is half padding.
	var samples bytes.Buffer
	m := newMerger(4, 2, &samples)
	if err := m.add(batch(0, 40, 10, &neurov1.Marker{SampleIndex: 45, Label: "cue\x141", DurationSeconds: 0.25})); err != nil {
		t.Fatal(err)
	}
	f := &edfFile{
		sessionID: "u-1 2", start: time.Date(2026, 11, 13, 9, 5, 7, 0, time.UTC), rate: 4, names: []string{"Cz", "Fp1"},
		samples: m.samples, min: m.min, max: m.max, annotations: m.annotations,
	}
	var out bytes.Buffer
	if err := writeEDF(&out, f, &samples); err != nil {
		t.Fatal(err)
	}
	p := readEDF(t, out.Bytes())

	header := string(p.header)
	for _, want := range []string{"0       ", "Startdate 13-NOV-2026 u-1_2 X NeuroHalo", "13.11.2609.05.07", "EDF+C", "3       1       3   "} {
		if !strings.Contains(header, want) {
			t.Errorf("header lacks %q:\n%s", want, header)
		}
	}
	if got := strings.Join(p.fields["label"], ","); got != "Cz,Fp1,EDF Annotations" {
		t.Errorf("labels = %s", got)
	}
	if got := strings.Join(p.fields["unit"], ","); got != "uV,uV," {
		t.Errorf("units = %s", got)
	}

	// Values come back within half a step of the 16-bit scale; the padding is 0.
	for s, sign := range []float64{1, -1} {
		if len(p.signals[s]) != 12 {
			t.Fatalf("sensor %d has %d samples, want 12 (3 records of 4)", s, len(p.signals[s]))
		}
		pmin, pmax := p.fields["pmin"][s], p.fields["pmax"][s]
		lo, _ := strconv.ParseFloat(pmin, 64)
		hi, _ := strconv.ParseFloat(pmax, 64)
		step := (hi - lo) / 65535
		for i, got := range p.signals[s] {
			want := 0.0
			if i < 10 {
				want = sign * float64(40+i)
			}
			if math.Abs(got-want) > step/2+1e-9 {
				t.Errorf("sensor %d sample %d = %g, want %g (step %g)", s, i, got, want, step)
			}
		}
	}

	// Each record starts with its own time; the marker at 1.25 s is in record 1.
	wantTALs := []string{
		"+0||",
		"+1||", "+1.25~0.25|cue 1|",
		"+2||", "+2.5~0.5|" + noDataText + "|",
	}
	if strings.Join(p.tals, " ") != strings.Join(wantTALs, " ") {
		t.Errorf("annotations = %q, want %q", p.tals, wantTALs)
	}
}
