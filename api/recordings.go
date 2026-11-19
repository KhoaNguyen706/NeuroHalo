package main

// The recorder's files, as the api serves and deletes them. The recorder
// (recorder/) writes them; the api only reads the recordings table and the
// object store.

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/neurohalo/neurohalo/internal/recording"
)

// recordingJSON is the state of a session's recording, inside sessionJSON.
// Filled by: scanSession. Read by: the session page, which offers the
// download once Status is "ready".
type recordingJSON struct {
	Status  string  `json:"status"`  // recording | writing | ready | failed
	Bytes   int64   `json:"bytes"`   // the EDF file's size, once ready
	Seconds float64 `json:"seconds"` // the signal it holds, once ready
}

// downloadName turns a session ID into a safe file name (see safeName).
func downloadName(id string) string {
	return safeName(id) + ".edf"
}

// getRecording handles GET /sessions/{id}/recording.edf: the session's raw
// signal as an EDF+ file, streamed from the object store. It answers 404 when
// nothing was recorded and 409 while the file is not written yet (or could
// not be, with the reason). Range requests work, so a large download can resume.
func (a *api) getRecording(w http.ResponseWriter, r *http.Request) {
	if a.objects == nil {
		writeError(w, http.StatusServiceUnavailable, "the object store is not configured")
		return
	}
	id := r.PathValue("id")
	var status, key, reason string
	err := a.db.QueryRow(r.Context(), `SELECT status, object_key, error FROM recordings WHERE session_id = $1`, id).
		Scan(&status, &key, &reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "nothing was recorded for this session")
		return
	case err != nil:
		serverError(w, r, err)
		return
	case status == "failed":
		writeError(w, http.StatusConflict, "the recording could not be written: "+reason)
		return
	case status != "ready":
		writeError(w, http.StatusConflict, "the recording is still being written")
		return
	}

	obj, err := a.objects.client.GetObject(r.Context(), a.objects.bucket, key, minio.GetObjectOptions{})
	if err == nil {
		defer obj.Close()
	}
	var info minio.ObjectInfo
	if err == nil {
		info, err = obj.Stat()
	}
	if err != nil {
		log.Printf("recording %s: %v", key, err)
		writeError(w, http.StatusServiceUnavailable, "file storage is unreachable, try again")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName(id)+`"`)
	http.ServeContent(w, r, "", info.LastModified, obj)
}

// removeRecording deletes a session's recording files: the EDF file and
// every chunk. Called after the rows are gone, so a failure is only logged.
// Used by: api.deleteSession.
func (a *api) removeRecording(ctx context.Context, id string) {
	objects := make(chan minio.ObjectInfo)
	go func() {
		defer close(objects)
		objects <- minio.ObjectInfo{Key: recording.FileKey(id)}
		for obj := range a.objects.client.ListObjects(ctx, a.objects.bucket,
			minio.ListObjectsOptions{Prefix: recording.SpoolDir(id), Recursive: true}) {
			if obj.Err != nil {
				log.Printf("delete session %s: cannot list its chunks: %v", id, obj.Err)
				return
			}
			objects <- obj
		}
	}()
	for e := range a.objects.client.RemoveObjects(ctx, a.objects.bucket, objects, minio.RemoveObjectsOptions{}) {
		log.Printf("delete session %s: cannot remove %s: %v", id, e.ObjectName, e.Err)
	}
}
