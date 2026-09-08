package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestOperationalAndUnimplementedRoutes(t *testing.T) {
	handler := NewHandler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/livez", 200}, {"GET", "/readyz", 200}, {"GET", "/", 200},
		{"POST", "/livez", 405}, {"POST", "/api/v1/events", 404}, {"GET", "/missing", 404},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d; want %d", response.Code, tc.status)
			}
		})
	}
}
