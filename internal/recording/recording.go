// Package recording names the objects the recorder stores for a session, so
// the recorder that writes them and the api that serves and deletes them
// agree on where they are.
package recording

import (
	"net/url"
	"strings"
)

// SpoolDir is the folder of a session's chunks: "spool/<id>/". The ID is
// escaped, so a "/" in it cannot make one session's folder contain another's.
// Used by: the recorder (chunkKey, finalize) and the api (removeRecording).
func SpoolDir(sessionID string) string {
	return "spool/" + url.PathEscape(sessionID) + "/"
}

// FileKey is where a session's EDF file is stored: "recordings/<id>.edf".
// Used by: the recorder (finalize) and the api (removeRecording).
func FileKey(sessionID string) string {
	return "recordings/" + url.PathEscape(sessionID) + ".edf"
}

// EDFLabel is how a sensor's name appears in the EDF file's header: printable
// ASCII only (anything else becomes '_'), at most 16 characters, and without
// the trailing spaces a reader strips. The BIDS export lists sensors under
// these names, so they match what EEGLAB or MNE read from the file.
// Used by: the recorder (writeEDF) and the api (the BIDS channels.tsv).
func EDFLabel(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '_'
		}
		return r
	}, name)
	if len(name) > 16 {
		name = name[:16]
	}
	return strings.TrimRight(name, " ")
}
