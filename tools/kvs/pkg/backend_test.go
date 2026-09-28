package pkg

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIBackendMountPath checks that api:// backends are mount-point aware:
// the handler is written against "/" and the route prefix is stripped before
// dispatch, so the same api:// service works at any path (page at the mount
// path, API at <mount>/__manager*) and root mounts behave unchanged.
func TestAPIBackendMountPath(t *testing.T) {
	SetZcodeHome(t.TempDir())

	get := func(b Backend, path string) *httptest.ResponseRecorder {
		h := CreateBackendHandler(b, nil, false)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	// Mounted at /admin: page at /admin (and /admin/), API under /admin/__manager*.
	admin := Backend{Source: "/admin", Scheme: "api", Target: "manager", RawURL: "api://manager"}
	if rec := get(admin, "/admin"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET /admin -> %d, want 200 page", rec.Code)
	}
	if rec := get(admin, "/admin/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET /admin/ -> %d, want 200 page", rec.Code)
	}
	if rec := get(admin, "/admin/__manager"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("GET /admin/__manager -> %d, want 200 json", rec.Code)
	}
	// A path that merely shares the prefix must not leak the handler.
	if rec := get(admin, "/administrator"); rec.Code != 404 {
		t.Fatalf("GET /administrator -> %d, want 404", rec.Code)
	}

	// Mounted at the root: no strip, page at / and API at /__manager.
	root := Backend{Source: "/", Scheme: "api", Target: "manager", RawURL: "api://manager"}
	if rec := get(root, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET / -> %d, want 200 page", rec.Code)
	}
	if rec := get(root, "/__manager"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("GET /__manager -> %d, want 200 json", rec.Code)
	}
}
