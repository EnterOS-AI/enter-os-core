package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// closeNotifyRecorder wraps httptest.ResponseRecorder with a no-op
// CloseNotify so httputil.ReverseProxy doesn't panic when served
// through Gin (which casts the writer to http.CloseNotifier).
type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
}

func (c *closeNotifyRecorder) CloseNotify() <-chan bool {
	return make(chan bool)
}

func newTestRecorder() *closeNotifyRecorder {
	return &closeNotifyRecorder{httptest.NewRecorder()}
}

func TestCanvasProxy_StripsAuthorizationHeader(t *testing.T) {
	var capturedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newCanvasProxy(upstream.URL))

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/some-canvas-page", nil)
	req.Header.Set("Authorization", "Bearer ws-secret-token")
	engine.ServeHTTP(w, req)

	if capturedAuth != "" {
		t.Errorf("Authorization header must not reach canvas upstream, got %q", capturedAuth)
	}
}

func TestCanvasProxy_StripsCookieHeader(t *testing.T) {
	var capturedCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newCanvasProxy(upstream.URL))

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/canvas-route", nil)
	req.Header.Set("Cookie", "session=abc123; auth=secret")
	engine.ServeHTTP(w, req)

	if capturedCookie != "" {
		t.Errorf("Cookie header must not reach canvas upstream, got %q", capturedCookie)
	}
}

func TestCanvasProxy_ForwardsOtherHeaders(t *testing.T) {
	var capturedAccept, capturedRequestID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAccept = r.Header.Get("Accept")
		capturedRequestID = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newCanvasProxy(upstream.URL))

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/page", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("X-Request-Id", "trace-abc")
	req.Header.Set("Authorization", "Bearer should-be-stripped")
	engine.ServeHTTP(w, req)

	if capturedAccept != "text/html" {
		t.Errorf("Accept header should be forwarded, got %q", capturedAccept)
	}
	if capturedRequestID != "trace-abc" {
		t.Errorf("X-Request-Id should be forwarded, got %q", capturedRequestID)
	}
}

func TestCanvasProxy_NoBothCredentialHeaders(t *testing.T) {
	var gotAuth, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newCanvasProxy(upstream.URL))

	w := newTestRecorder()
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.Header.Set("Authorization", "Bearer token123")
	req.Header.Set("Cookie", "sid=xyz")
	engine.ServeHTTP(w, req)

	if gotAuth != "" || gotCookie != "" {
		t.Errorf("both credential headers must be stripped: Authorization=%q Cookie=%q", gotAuth, gotCookie)
	}
}
