package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeEDF builds a minimal EDF file: the given labels, samples per record for
// each, and the number of records, with zero-filled data.
func fakeEDF(version string, labels []string, perRecord, records int) []byte {
	n := len(labels)
	pad := func(s string, w int) string { return fmt.Sprintf("%-*s", w, s)[:w] }
	var b strings.Builder
	b.WriteString(pad(version, 8))
	b.WriteString(pad("patient", 80))
	b.WriteString(pad("recording", 80))
	b.WriteString(pad("01.01.26", 8))
	b.WriteString(pad("00.00.00", 8))
	b.WriteString(pad(fmt.Sprint(256*(n+1)), 8))
	b.WriteString(pad("", 44))
	b.WriteString(pad(fmt.Sprint(records), 8))
	b.WriteString(pad("1", 8))
	b.WriteString(pad(fmt.Sprint(n), 4))
	each := func(w int, v func(i int) string) {
		for i := 0; i < n; i++ {
			b.WriteString(pad(v(i), w))
		}
	}
	each(16, func(i int) string { return labels[i] })
	each(80, func(int) string { return "" })
	each(8, func(int) string { return "uV" })
	each(8, func(int) string { return "-1000" })
	each(8, func(int) string { return "1000" })
	each(8, func(int) string { return "-32768" })
	each(8, func(int) string { return "32767" })
	each(80, func(int) string { return "" })
	each(8, func(int) string { return fmt.Sprint(perRecord) })
	each(32, func(int) string { return "" })
	return append([]byte(b.String()), make([]byte, n*perRecord*2*records)...)
}

// TestParseEDFHeader checks a valid file, a truncated one and a non-EDF one.
func TestParseEDFHeader(t *testing.T) {
	good := fakeEDF("0", []string{"Cz..", "Oz..", "EDF Annotations"}, 160, 3)
	info, err := parseEDFHeader(bytes.NewReader(good), int64(len(good)))
	if err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
	if len(info.Signals) != 2 || info.SampleRateHz != 160 || info.DurationSeconds != 3 {
		t.Errorf("got %+v, want 2 signals at 160 Hz for 3 s", info)
	}

	cut := good[:len(good)-100]
	if _, err := parseEDFHeader(bytes.NewReader(cut), int64(len(cut))); err == nil {
		t.Error("truncated file accepted")
	}
	bdf := fakeEDF("BIOSEMI", []string{"Cz"}, 160, 1)
	if _, err := parseEDFHeader(bytes.NewReader(bdf), int64(len(bdf))); err == nil {
		t.Error("non-EDF file accepted")
	}
	if _, err := parseEDFHeader(bytes.NewReader([]byte("hello")), 5); err == nil {
		t.Error("tiny file accepted")
	}
}

// TestParseRealEDF reads a downloaded sample, when it is present.
func TestParseRealEDF(t *testing.T) {
	data, err := os.ReadFile("../data/S001R01.edf")
	if err != nil {
		t.Skip("sample recording not downloaded")
	}
	info, err := parseEDFHeader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Signals) != 64 || info.SampleRateHz != 160 || info.DurationSeconds != 61 {
		t.Errorf("got %d signals at %g Hz for %g s, want 64 at 160 Hz for 61 s",
			len(info.Signals), info.SampleRateHz, info.DurationSeconds)
	}
}
