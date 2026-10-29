package operator

import (
	"io"
	"net/http"
	"strconv"

	"github.com/ang-ee/angee-operator/api"
)

func (s *Server) backupPreview(w http.ResponseWriter, r *http.Request) {
	req, err := decodeStrict[api.BackupRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	preview, err := s.platform.BackupPreview(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}
func (s *Server) backupStart(w http.ResponseWriter, r *http.Request) {
	req, err := decodeStrict[api.BackupRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	receipt, err := s.platform.BackupStart(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
}
func (s *Server) backupArchive(w http.ResponseWriter, r *http.Request) {
	file, artifact, err := s.platform.BackupArchive(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="backup.zip"`)
	w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
	w.Header().Set("ETag", `"`+artifact.SHA256+`"`)
	_, _ = io.Copy(w, file)
}
