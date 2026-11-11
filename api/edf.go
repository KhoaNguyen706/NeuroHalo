package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// edfInfo is what an upload's header says, returned to the uploader.
// Built by: parseEDFHeader.
// Used by: api.upload.
type edfInfo struct {
	Signals         []string `json:"signals"` // labels, without the annotation channel
	SampleRateHz    float64  `json:"sampleRateHz"`
	DurationSeconds float64  `json:"durationSeconds"`
}

const maxEDFSignals = 1024

// parseEDFHeader reads and checks the header of an EDF file without reading
// the samples. It rejects anything that is not plain EDF (EDF+ is fine, BDF is
// not), has inconsistent sizes, or is shorter than its header promises.
func parseEDFHeader(r io.ReaderAt, size int64) (*edfInfo, error) {
	fixed := make([]byte, 256)
	if _, err := r.ReadAt(fixed, 0); err != nil {
		return nil, errors.New("file is shorter than an EDF header")
	}
	field := func(b []byte, from, n int) string { return strings.TrimSpace(string(b[from : from+n])) }
	number := func(b []byte, from, n int, name string) (float64, error) {
		v, err := strconv.ParseFloat(field(b, from, n), 64)
		if err != nil {
			return 0, fmt.Errorf("header field %s is not a number: %q", name, field(b, from, n))
		}
		return v, nil
	}

	if field(fixed, 0, 8) != "0" {
		return nil, errors.New("not an EDF file (the version field must be 0)")
	}
	headerBytes, err := number(fixed, 184, 8, "header size")
	if err != nil {
		return nil, err
	}
	records, err := number(fixed, 236, 8, "number of records")
	if err != nil {
		return nil, err
	}
	recordSeconds, err := number(fixed, 244, 8, "record duration")
	if err != nil {
		return nil, err
	}
	ns, err := number(fixed, 252, 4, "number of signals")
	if err != nil {
		return nil, err
	}
	n := int(ns)
	switch {
	case n < 1 || n > maxEDFSignals:
		return nil, fmt.Errorf("number of signals %d is outside 1 to %d", n, maxEDFSignals)
	case int64(headerBytes) != int64(256*(n+1)):
		return nil, fmt.Errorf("header size %g does not match %d signals", headerBytes, n)
	case !(recordSeconds > 0):
		return nil, errors.New("record duration must be positive")
	case size < int64(headerBytes):
		return nil, errors.New("file is shorter than its header")
	}

	sig := make([]byte, 256*n)
	if _, err := r.ReadAt(sig, 256); err != nil {
		return nil, errors.New("file is shorter than its signal headers")
	}
	// Signal header layout: label 16, transducer 80, unit 8, physical min/max
	// 8+8, digital min/max 8+8, prefiltering 80, samples per record 8, reserved 32.
	samplesAt := n * (16 + 80 + 8 + 8 + 8 + 8 + 8 + 80)
	var recordBytes int64
	info := &edfInfo{}
	for i := 0; i < n; i++ {
		perRecord, err := number(sig, samplesAt+8*i, 8, "samples per record")
		if err != nil {
			return nil, err
		}
		if perRecord < 1 {
			return nil, fmt.Errorf("signal %d has no samples", i+1)
		}
		recordBytes += int64(perRecord) * 2
		label := field(sig, 16*i, 16)
		if label == "EDF Annotations" {
			continue
		}
		info.Signals = append(info.Signals, label)
		info.SampleRateHz = max(info.SampleRateHz, perRecord/recordSeconds)
	}
	if len(info.Signals) == 0 {
		return nil, errors.New("file has no signals besides annotations")
	}

	dataBytes := size - int64(headerBytes)
	if records == -1 { // allowed while recording; work it out from the size
		records = float64(dataBytes / recordBytes)
	}
	if records < 1 {
		return nil, errors.New("file holds no data records")
	}
	if int64(records)*recordBytes > dataBytes {
		return nil, fmt.Errorf("header promises %g records but the file is too short for them", records)
	}
	info.DurationSeconds = records * recordSeconds
	return info, nil
}
