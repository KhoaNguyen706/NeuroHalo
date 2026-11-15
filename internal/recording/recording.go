// Package recording names the objects the recorder stores for a session, so
// the recorder that writes them and the api that serves and deletes them
// agree on where they are.
package recording

import "net/url"

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
