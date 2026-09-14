package httpapi

import (
	"crypto/sha256"
	"net/http"
)

func (a api) replay(w http.ResponseWriter, r *http.Request, client string) {
	if !uuid.MatchString(r.PathValue("id")) {
		problem(w, 404, "not found")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !keyPattern.MatchString(keys[0]) {
		problem(w, 400, "a valid Idempotency-Key is required")
		return
	}
	var input struct {
		AcknowledgeDuplicateRisk bool `json:"acknowledge_duplicate_risk"`
	}
	raw, err := readJSON(w, r, &input)
	if err != nil {
		invalidBody(w, err)
		return
	}
	if !input.AcknowledgeDuplicateRisk {
		problem(w, 400, "acknowledge_duplicate_risk must be true; receivers must deduplicate by event ID")
		return
	}
	receipt, duplicate, err := a.db.ReplayDelivery(r.Context(), client, r.PathValue("id"), keys[0], sha256.Sum256(raw))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	respond(w, status, receipt)
}
