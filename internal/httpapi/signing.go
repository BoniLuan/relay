package httpapi

import (
	"net/http"
	"strconv"
)

func secretIDs(w http.ResponseWriter, r *http.Request, withVersion bool) (string, int, bool) {
	id := r.PathValue("id")
	if !uuid.MatchString(id) {
		problem(w, 404, "not found")
		return "", 0, false
	}
	if !withVersion {
		return id, 0, true
	}
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 32)
	if err != nil || version < 1 {
		problem(w, 404, "not found")
		return "", 0, false
	}
	return id, int(version), true
}
func (a api) stageSecret(w http.ResponseWriter, r *http.Request, client string) {
	id, _, ok := secretIDs(w, r, false)
	if !ok {
		return
	}
	var input struct{}
	if _, err := readJSON(w, r, &input); err != nil {
		invalidBody(w, err)
		return
	}
	meta, secret, err := a.db.StageSigningSecret(r.Context(), client, id)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	// Deliberately the only HTTP response that serializes plaintext credentials.
	respond(w, 201, map[string]any{"version": meta.Version, "state": meta.State, "created_at": meta.CreatedAt, "secret": secret.Export()})
}
func (a api) listSecrets(w http.ResponseWriter, r *http.Request, client string) {
	id, _, ok := secretIDs(w, r, false)
	if !ok {
		return
	}
	result, err := a.db.ListSigningSecrets(r.Context(), client, id)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	respond(w, 200, map[string]any{"secrets": result})
}
func (a api) activateSecret(w http.ResponseWriter, r *http.Request, client string) {
	id, version, ok := secretIDs(w, r, true)
	if !ok {
		return
	}
	var input struct{}
	if _, err := readJSON(w, r, &input); err != nil {
		invalidBody(w, err)
		return
	}
	if err := a.db.ActivateSigningSecret(r.Context(), client, id, version); err != nil {
		a.failure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(204)
}
func (a api) revokeSecret(w http.ResponseWriter, r *http.Request, client string) {
	id, version, ok := secretIDs(w, r, true)
	if !ok {
		return
	}
	if err := a.db.RevokeSigningSecret(r.Context(), client, id, version); err != nil {
		a.failure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(204)
}
