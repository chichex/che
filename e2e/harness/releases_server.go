package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// ReleasesServer is a local httptest.Server that impersonates GitHub's
// /releases/latest endpoint plus optional asset downloads. Tests configure
// the response via SetLatest or SetStatus and the server writes the URL
// into the env as CHE_RELEASES_API_URL automatically.
type ReleasesServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	tag    string
	assets map[string][]byte
	status int
	errMsg string
}

// StartReleasesServer launches the fake GitHub releases server for this test.
// The server is registered for teardown when the test ends.
func (e *Env) StartReleasesServer() *ReleasesServer {
	e.t.Helper()
	rs := &ReleasesServer{assets: map[string][]byte{}}
	rs.srv = httptest.NewServer(http.HandlerFunc(rs.serve))
	e.SetEnv("CHE_RELEASES_API_URL", rs.srv.URL+"/releases/latest")
	e.registerCleanup(rs.srv.Close)
	return rs
}

// SetLatest configures the response to the /releases/latest endpoint.
// assets maps filename → body; browser_download_url will point back to
// this server at /assets/<filename>.
func (rs *ReleasesServer) SetLatest(tag string, assets map[string][]byte) *ReleasesServer {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.tag = tag
	rs.assets = assets
	if rs.assets == nil {
		rs.assets = map[string][]byte{}
	}
	return rs
}

// SetStatus forces /releases/latest to respond with the given status and body.
func (rs *ReleasesServer) SetStatus(code int, body string) *ReleasesServer {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.status = code
	rs.errMsg = body
	return rs
}

// URL returns the base URL of the fake server.
func (rs *ReleasesServer) URL() string { return rs.srv.URL }

func (rs *ReleasesServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if strings.HasPrefix(r.URL.Path, "/assets/") {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		body, ok := rs.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
		return
	}

	if r.URL.Path != "/releases/latest" {
		http.NotFound(w, r)
		return
	}

	if rs.status != 0 {
		w.WriteHeader(rs.status)
		_, _ = w.Write([]byte(rs.errMsg))
		return
	}

	type asset struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	}
	type release struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}
	resp := release{TagName: rs.tag}
	for name := range rs.assets {
		resp.Assets = append(resp.Assets, asset{
			Name:        name,
			DownloadURL: rs.srv.URL + "/assets/" + name,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
