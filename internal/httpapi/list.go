package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/BoniLuan/relay/internal/storage"
)

// The cursor is a validated position, not a credential or a signed capability.
// Every database lookup independently enforces authenticated ownership.
type deliveryCursor struct {
	Version       int                      `json:"v"`
	Client        string                   `json:"client"`
	Status        string                   `json:"status"`
	DestinationID string                   `json:"destination_id"`
	Position      storage.DeliveryPosition `json:"position"`
}

type deliveryPage struct {
	Items      []storage.DeliverySummary `json:"items"`
	NextCursor *string                   `json:"next_cursor"`
}

func parseDeliveryFilter(rawQuery, client string) (storage.DeliveryFilter, error) {
	f := storage.DeliveryFilter{Limit: 20}
	bad := errors.New("invalid delivery query")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return f, bad
	}
	for key, values := range q {
		if len(values) != 1 || values[0] == "" {
			return f, bad
		}
		switch key {
		case "limit", "status", "destination_id", "cursor":
		default:
			return f, bad
		}
	}
	if value := q.Get("limit"); value != "" {
		for _, c := range value {
			if c < '0' || c > '9' {
				return f, bad
			}
		}
		f.Limit, err = strconv.Atoi(value)
		if err != nil || f.Limit < 1 || f.Limit > 100 {
			return f, bad
		}
	}
	f.Status = q.Get("status")
	switch f.Status {
	case "", "pending", "leased", "attempting", "retry_wait", "succeeded", "failed", "unknown":
	default:
		return f, bad
	}
	f.DestinationID = q.Get("destination_id")
	if f.DestinationID != "" && !uuid.MatchString(f.DestinationID) {
		return f, bad
	}
	if value := q.Get("cursor"); value != "" {
		if len(value) > 1024 {
			return f, bad
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
		if err != nil {
			return f, bad
		}
		var c deliveryCursor
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&c); err != nil {
			return f, bad
		}
		if err = decoder.Decode(new(any)); err != io.EOF {
			return f, bad
		}
		if c.Version != 1 || c.Client != client || c.Status != f.Status || c.DestinationID != f.DestinationID || !uuid.MatchString(c.Position.EventID) || c.Position.CreatedAt.IsZero() || c.Position.CreatedAt.Year() < 1 || c.Position.CreatedAt.Year() > 9999 {
			return f, bad
		}
		f.Before = &c.Position
	}
	return f, nil
}

func (a api) deliveries(w http.ResponseWriter, r *http.Request, client string) {
	filter, err := parseDeliveryFilter(r.URL.RawQuery, client)
	if err != nil {
		problem(w, 400, "invalid delivery query; use limit 1-100, a valid status/destination_id and a matching cursor")
		return
	}
	items, more, err := a.db.ListDeliveries(r.Context(), client, filter)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	page := deliveryPage{Items: items}
	if more {
		last := items[len(items)-1]
		encoded, err := json.Marshal(deliveryCursor{Version: 1, Client: client, Status: filter.Status, DestinationID: filter.DestinationID, Position: storage.DeliveryPosition{CreatedAt: last.CreatedAt, EventID: last.EventID}})
		if err != nil {
			a.failure(w, r, err)
			return
		}
		cursor := base64.RawURLEncoding.EncodeToString(encoded)
		page.NextCursor = &cursor
	}
	respond(w, 200, page)
}
