package httpapi

import "net/http"

func (a api) history(w http.ResponseWriter, r *http.Request, client string) {
	if !uuid.MatchString(r.PathValue("id")) {
		problem(w, http.StatusNotFound, "not found")
		return
	}
	history, err := a.db.GetDeliveryHistory(r.Context(), client, r.PathValue("id"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	respond(w, http.StatusOK, history)
}
