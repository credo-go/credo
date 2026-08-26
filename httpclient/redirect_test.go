package httpclient_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/credo-go/credo/httpclient"
)

func TestNew_QUERYRedirect301302PreservesMethodBodyAndHeaders(t *testing.T) {
	var (
		gotMethod  string
		gotBody    string
		gotHeaders http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusMovedPermanently)
		case "/middle":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			gotMethod = r.Method
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("ReadAll: %v", err)
			}
			gotBody = string(body)
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "identity")
	req.Header.Add("Content-Language", "en")
	req.Header.Add("Content-Language", "tr")
	req.Header.Set("Content-Location", "/queries/current")

	resp, err := httpclient.New().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if gotMethod != "QUERY" || gotBody != "payload" {
		t.Fatalf("final method/body = %q/%q, want QUERY/payload", gotMethod, gotBody)
	}
	wantHeaders := map[string][]string{
		"Content-Type":     {"application/json"},
		"Content-Encoding": {"identity"},
		"Content-Language": {"en", "tr"},
		"Content-Location": {"/queries/current"},
	}
	for name, want := range wantHeaders {
		got := gotHeaders.Values(name)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestNew_QUERYRedirectBodylessPreservesContentHeaders(t *testing.T) {
	var gotMethod, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)
			return
		}
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpclient.New().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if gotMethod != "QUERY" || gotContentType != "application/json" {
		t.Fatalf("final method/Content-Type = %q/%q, want QUERY/application/json", gotMethod, gotContentType)
	}
}

func TestNew_QUERYRedirectNonReplayableReturnsLastResponse(t *testing.T) {
	var targetHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		targetHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start",
		io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if req.GetBody != nil {
		t.Fatal("precondition failed: GetBody is set")
	}

	resp, err := httpclient.New().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want original 302", resp.StatusCode)
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect target was called for non-replayable QUERY")
	}
}

type closeTrackingBody struct {
	closed *atomic.Bool
}

func (*closeTrackingBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestNew_QUERYRedirectGetBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start",
		io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	closed := new(atomic.Bool)
	wantErr := errors.New("cannot replay")
	req.GetBody = func() (io.ReadCloser, error) {
		return &closeTrackingBody{closed: closed}, wantErr
	}

	resp, err := httpclient.New().Do(req)
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Do error = %v, want wrapped %v", err, wantErr)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !closed.Load() {
		t.Fatal("body returned alongside GetBody error was not closed")
	}
}

func TestNew_QUERYRedirect303StaysGETAcrossLaterRedirect(t *testing.T) {
	var methodsMu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methodsMu.Lock()
		methods = append(methods, r.Method)
		methodsMu.Unlock()
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/after-303", http.StatusSeeOther)
		case "/after-303":
			http.Redirect(w, r, "/final", http.StatusMovedPermanently)
		case "/final":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpclient.New().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	methodsMu.Lock()
	defer methodsMu.Unlock()
	if got := strings.Join(methods, ","); got != "QUERY,GET,GET" {
		t.Fatalf("redirect methods = %s, want QUERY,GET,GET", got)
	}
}

func TestNew_QUERYRedirect307And308PreserveMethodAndBody(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var gotMethod, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, "/final", status)
					return
				}
				gotMethod = r.Method
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)

			req, err := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL+"/start", strings.NewReader("payload"))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpclient.New().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if gotMethod != "QUERY" || gotBody != "payload" {
				t.Fatalf("method/body = %q/%q, want QUERY/payload", gotMethod, gotBody)
			}
		})
	}
}

func TestNew_QUERYRedirectDoesNotRestoreSensitiveHeadersCrossOrigin(t *testing.T) {
	gotHeaders := make(chan http.Header, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("Parse target URL: %v", err)
	}
	_, port, err := net.SplitHostPort(targetURL.Host)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	targetURL.Host = net.JoinHostPort("localhost", port)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetURL.String(), http.StatusFound)
	}))
	t.Cleanup(source.Close)

	req, err := http.NewRequestWithContext(t.Context(), "QUERY", source.URL, strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"Authorization", "Www-Authenticate", "Cookie", "Cookie2", "Proxy-Authorization", "Proxy-Authenticate"} {
		req.Header.Set(name, "secret")
	}
	resp, err := httpclient.New().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	headers := <-gotHeaders
	for _, name := range []string{"Authorization", "Www-Authenticate", "Cookie", "Cookie2", "Proxy-Authorization", "Proxy-Authenticate"} {
		if got := headers.Get(name); got != "" {
			t.Errorf("%s leaked across origin: %q", name, got)
		}
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestNew_NonQUERYRedirectBehaviorUnchanged(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	resp, err := httpclient.New().Post(srv.URL+"/start", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if gotMethod != http.MethodGet {
		t.Fatalf("redirected POST method = %q, want GET", gotMethod)
	}
}

func TestNew_RedirectLimitRetained(t *testing.T) {
	for _, method := range []string{http.MethodGet, "QUERY"} {
		t.Run(method, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Redirect(w, r, "/loop", http.StatusMovedPermanently)
			}))
			t.Cleanup(srv.Close)

			var body io.Reader
			if method == "QUERY" {
				body = strings.NewReader("payload")
			}
			req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+"/loop", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if method == "QUERY" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := httpclient.New().Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
				t.Fatalf("Do error = %v, want redirect-limit error", err)
			}
			if got := hits.Load(); got != 10 {
				t.Fatalf("server hits = %d, want 10", got)
			}
		})
	}
}
