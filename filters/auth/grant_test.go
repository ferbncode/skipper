package auth_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/filters/auth"
	"github.com/zalando/skipper/filters/builtin"
	"github.com/zalando/skipper/proxy/proxytest"
	"github.com/zalando/skipper/routing"
	"github.com/zalando/skipper/secrets"
)

const (
	testToken      = "foobarbaz"
	testAccessCode = "quxquuxquz"
	testSecretFile = "testdata/authsecret"
)

func newTestTokeninfo(validToken string) *httptest.Server {
	const prefix = "Bearer "
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := func(code int) {
			w.WriteHeader(code)
			w.Write([]byte("{}"))
		}

		token := r.Header.Get("Authorization")
		if !strings.HasPrefix(token, prefix) || token[len(prefix):] != validToken {
			response(http.StatusUnauthorized)
			return
		}

		response(http.StatusOK)
	}))
}

func newTestAuthServer(testToken, testAccessCode string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := func(w http.ResponseWriter, r *http.Request) {
			rq := r.URL.Query()
			redirect := rq.Get("redirect_uri")
			rd, err := url.Parse(redirect)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			q := rd.Query()
			q.Set("code", testAccessCode)
			q.Set("state", r.URL.Query().Get("state"))
			rd.RawQuery = q.Encode()

			http.Redirect(
				w,
				r,
				rd.String(),
				http.StatusTemporaryRedirect,
			)
		}

		token := func(w http.ResponseWriter, r *http.Request) {
			code := r.FormValue("code")
			if code != testAccessCode {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			type tokenJSON struct {
				AccessToken string `json:"access_token"`
				ExpiresIn   int    `json:"expires_in"`
			}

			token := tokenJSON{
				AccessToken: testToken,
				ExpiresIn:   int(time.Hour / time.Second),
			}

			b, err := json.Marshal(token)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		}

		switch r.URL.Path {
		case "/auth":
			auth(w, r)
		case "/token":
			token(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newAuthProxy(tokeninfoURL, providerURL string) (*proxytest.TestProxy, error) {
	config := &auth.OAuthConfig{
		Secrets:      secrets.NewRegistry(),
		SecretFile:   testSecretFile,
		TokeninfoURL: tokeninfoURL,
		AuthURL:      providerURL + "/auth",
		TokenURL:     providerURL + "/token",
	}

	grantSpec, err := config.NewGrant()
	if err != nil {
		return nil, err
	}

	grantCallbackSpec, err := config.NewGrantCallback()
	if err != nil {
		return nil, err
	}

	grantPrep, err := config.NewGrantPreprocessor()
	if err != nil {
		return nil, err
	}

	fr := builtin.MakeRegistry()
	fr.Register(grantSpec)
	fr.Register(grantCallbackSpec)

	ro := routing.Options{
		PreProcessors: []routing.PreProcessor{grantPrep},
	}

	return proxytest.WithRoutingOptions(fr, ro, &eskip.Route{
		Filters: []*eskip.Filter{
			{Name: auth.OAuthGrantName},
			{Name: "status", Args: []interface{}{http.StatusNoContent}},
		},
		BackendType: eskip.ShuntBackend,
	}), nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func checkStatus(t *testing.T, rsp *http.Response, expectedStatus int) {
	if rsp.StatusCode != expectedStatus {
		t.Fatalf(
			"Unexpected status code, got: %d, expected: %d.",
			rsp.StatusCode,
			expectedStatus,
		)
	}
}

func checkRedirect(t *testing.T, rsp *http.Response, expectedURL string) {
	checkStatus(t, rsp, http.StatusTemporaryRedirect)
	redirectTo := rsp.Header.Get("Location")
	if !strings.HasPrefix(redirectTo, expectedURL) {
		t.Fatalf(
			"Unexpected redirect location, got: '%s', expected: '%s'.",
			redirectTo,
			expectedURL,
		)
	}
}

func findAuthCookie(rsp *http.Response) (*http.Cookie, bool) {
	for _, c := range rsp.Cookies() {
		if c.Name == auth.OAuthGrantCookieName {
			return c, true
		}
	}

	return nil, false
}

func checkCookie(t *testing.T, rsp *http.Response) {
	c, ok := findAuthCookie(rsp)
	if !ok {
		t.Fatalf("Cookie not found.")
	}

	if c.Value == "" {
		t.Fatalf("Cookie deleted.")
	}
}

func TestGrantFlow(t *testing.T) {
	t.Log("create a test provider")
	provider := newTestAuthServer(testToken, testAccessCode)
	defer provider.Close()

	t.Log("create a test tokeninfo")
	tokeninfo := newTestTokeninfo(testToken)
	defer tokeninfo.Close()

	t.Log("create a proxy, returning 204, oauthGrant filter, initially without parameters")
	proxy, err := newAuthProxy(tokeninfo.URL, provider.URL)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("create a client without redirects, to check it manually")
	client := newHTTPClient()

	t.Log("make a request to the proxy without a cookie")
	rsp, err := client.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	defer rsp.Body.Close()

	t.Log("get redirected to the auth endpoint")
	checkRedirect(t, rsp, provider.URL+"/auth")

	t.Log("follow the redirect")
	rsp, err = client.Get(rsp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Failed to make request to provider: %v.", err)
	}

	defer rsp.Body.Close()

	t.Log("get redirected back to the proxy callback URL")
	checkRedirect(t, rsp, proxy.URL+"/.well-known/oauth2-callback")

	t.Log("follow the redirect")
	rsp, err = client.Get(rsp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("Failed to make request to proxy: %v.", err)
	}

	defer rsp.Body.Close()

	t.Log("get redirected back to the proxy")
	checkRedirect(t, rsp, proxy.URL)

	t.Log("check auth cookie was set")
	checkCookie(t, rsp)

	t.Log("follow the redirect, with the cookie")
	req, err := http.NewRequest("GET", rsp.Header.Get("Location"), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v.", err)
	}

	c, _ := findAuthCookie(rsp)
	req.Header.Set("Cookie", fmt.Sprintf("%s=%s", c.Name, c.Value))
	rsp, err = client.Do(req)
	if err != nil {
		t.Fatalf("Failed to make request to proxy: %v.", err)
	}

	t.Log("check for successful request")
	checkStatus(t, rsp, http.StatusNoContent)
}

func TestGrantRefresh(t *testing.T) {
}