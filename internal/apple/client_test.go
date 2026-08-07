package apple

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testResponse(request *http.Request, status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("Content-Type") == "" && body != "" {
		headers.Set("Content-Type", "application/json")
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d test", status),
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func deterministicRandom() io.Reader {
	data := make([]byte, 128)
	for index := range data {
		data[index] = byte(index + 1)
	}
	return bytes.NewReader(data)
}

type authFixture struct {
	t                    *testing.T
	completeStatus       int
	password             string
	appleID              string
	clientPublic         *big.Int
	serverPublic         *big.Int
	serverSecret         *big.Int
	salt                 []byte
	iterations           int
	requests             []string
	requestBodies        []string
	trustRequests        int
	accountLoginRequests int
	verifyStatus         int
}

func newAuthFixture(t *testing.T, completeStatus int) *authFixture {
	return &authFixture{
		t:              t,
		completeStatus: completeStatus,
		password:       "correct horse battery staple",
		appleID:        "owner@example.com",
		serverSecret:   big.NewInt(123456789),
		salt:           []byte("fixed-apple-salt"),
		iterations:     10,
		verifyStatus:   http.StatusConflict,
	}
}

func (fixture *authFixture) RoundTrip(request *http.Request) (*http.Response, error) {
	fixture.requests = append(fixture.requests, request.Method+" "+request.URL.Host+request.URL.Path)
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
	}
	fixture.requestBodies = append(fixture.requestBodies, string(body))

	switch request.URL.Path {
	case "/appleauth/auth/authorize/signin":
		if request.Method != http.MethodGet || request.URL.Query().Get("client_id") != defaultWidgetKey || request.URL.Query().Get("redirect_uri") != "https://www.icloud.com" {
			return nil, errors.New("invalid authorize request")
		}
		headers := make(http.Header)
		headers.Set("scnt", "scnt-one")
		headers.Set("X-Apple-ID-Session-Id", "session-one")
		headers.Set("X-Apple-Auth-Attributes", "attributes-one")
		headers.Add("Set-Cookie", "idms=initial; Domain=.apple.com; Path=/; Secure; HttpOnly")
		return testResponse(request, http.StatusOK, "", headers), nil

	case "/appleauth/auth/federate":
		if request.Header.Get("scnt") != "scnt-one" || request.Header.Get("X-Apple-ID-Session-Id") != "session-one" || request.Header.Get("X-Apple-Auth-Attributes") != "attributes-one" {
			return nil, errors.New("federate did not carry auth session headers")
		}
		var federate map[string]any
		if json.Unmarshal(body, &federate) != nil || federate["accountName"] != fixture.appleID {
			return nil, errors.New("invalid federate body")
		}
		headers := make(http.Header)
		headers.Set("scnt", "scnt-two")
		return testResponse(request, http.StatusOK, `{}`, headers), nil

	case "/appleauth/auth/signin/init":
		var initRequest struct {
			A           string   `json:"a"`
			AccountName string   `json:"accountName"`
			Protocols   []string `json:"protocols"`
		}
		if err := json.Unmarshal(body, &initRequest); err != nil {
			return nil, err
		}
		aBytes, err := base64.StdEncoding.DecodeString(initRequest.A)
		if err != nil || len(aBytes) != appleSRPSize {
			return nil, errors.New("invalid SRP A")
		}
		fixture.clientPublic = new(big.Int).SetBytes(aBytes)
		passwordKey, err := deriveApplePassword(fixture.password, fixture.salt, fixture.iterations, "s2k_fo")
		if err != nil {
			return nil, err
		}
		x := srpX(fixture.salt, passwordKey)
		verifier := new(big.Int).Exp(appleSRPG, x, appleSRPN)
		serverComponent := new(big.Int).Exp(appleSRPG, fixture.serverSecret, appleSRPN)
		fixture.serverPublic = new(big.Int).Add(new(big.Int).Mul(srpMultiplier(), verifier), serverComponent)
		fixture.serverPublic.Mod(fixture.serverPublic, appleSRPN)
		response := map[string]any{
			"salt":      base64.StdEncoding.EncodeToString(fixture.salt),
			"b":         base64.StdEncoding.EncodeToString(padSRP(fixture.serverPublic)),
			"c":         "challenge-token",
			"iteration": fixture.iterations,
			"protocol":  "s2k_fo",
		}
		encoded, _ := json.Marshal(response)
		return testResponse(request, http.StatusOK, string(encoded), nil), nil

	case "/appleauth/auth/signin/complete":
		var complete struct {
			AccountName string `json:"accountName"`
			M1          string `json:"m1"`
			M2          string `json:"m2"`
		}
		if err := json.Unmarshal(body, &complete); err != nil {
			return nil, err
		}
		if complete.AccountName != fixture.appleID {
			return nil, errors.New("wrong account in SRP complete")
		}
		passwordKey, _ := deriveApplePassword(fixture.password, fixture.salt, fixture.iterations, "s2k_fo")
		x := srpX(fixture.salt, passwordKey)
		verifier := new(big.Int).Exp(appleSRPG, x, appleSRPN)
		u := srpU(fixture.clientPublic, fixture.serverPublic)
		verifierU := new(big.Int).Exp(verifier, u, appleSRPN)
		sharedBase := new(big.Int).Mul(fixture.clientPublic, verifierU)
		sharedBase.Mod(sharedBase, appleSRPN)
		shared := new(big.Int).Exp(sharedBase, fixture.serverSecret, appleSRPN)
		key := srpHash(padSRP(shared))
		wantM1 := srpM1([]byte(fixture.appleID), fixture.salt, padSRP(fixture.clientPublic), padSRP(fixture.serverPublic), key)
		wantM2 := srpM2(padSRP(fixture.clientPublic), wantM1, key)
		gotM1, err := base64.StdEncoding.DecodeString(complete.M1)
		if err != nil || !bytes.Equal(gotM1, wantM1) {
			return nil, errors.New("m1 is not the SRP client proof M")
		}
		gotM2, err := base64.StdEncoding.DecodeString(complete.M2)
		if err != nil || !bytes.Equal(gotM2, wantM2) {
			return nil, errors.New("m2 is not the SRP server proof H_AMK")
		}
		headers := make(http.Header)
		headers.Set("X-Apple-ID-Account-Country", "US")
		if fixture.completeStatus == http.StatusOK {
			headers.Set("X-Apple-Session-Token", "session-token")
		}
		return testResponse(request, fixture.completeStatus, `{}`, headers), nil

	case "/appleauth/auth/verify/trusteddevice/securitycode":
		if request.Method == http.MethodPut {
			return testResponse(request, http.StatusMethodNotAllowed, `{}`, nil), nil
		}
		var verification struct {
			SecurityCode struct {
				Code string `json:"code"`
			} `json:"securityCode"`
		}
		if json.Unmarshal(body, &verification) != nil || verification.SecurityCode.Code != "123456" {
			return testResponse(request, http.StatusBadRequest, `{"errorCode":-21669}`, nil), nil
		}
		headers := make(http.Header)
		if fixture.verifyStatus == http.StatusConflict {
			headers.Set("X-Apple-Session-Token", "session-token")
		}
		return testResponse(request, fixture.verifyStatus, `{}`, headers), nil

	case "/appleauth/auth/2sv/trust":
		fixture.trustRequests++
		headers := make(http.Header)
		headers.Set("X-Apple-TwoSV-Trust-Token", "trust-token")
		return testResponse(request, http.StatusNoContent, "", headers), nil

	case "/setup/ws/1/accountLogin":
		fixture.accountLoginRequests++
		var login struct {
			SessionToken string `json:"dsWebAuthToken"`
			TrustToken   string `json:"trustToken"`
		}
		if err := json.Unmarshal(body, &login); err != nil || login.SessionToken != "session-token" {
			return nil, errors.New("invalid accountLogin token")
		}
		if fixture.completeStatus == http.StatusConflict && login.TrustToken != "trust-token" {
			return nil, errors.New("accountLogin omitted 2FA trust token")
		}
		headers := make(http.Header)
		headers.Add("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=web-token; Domain=.icloud.com; Path=/; Secure; HttpOnly")
		return testResponse(request, http.StatusOK, `{
			"dsInfo":{"dsid":123456789,"primaryEmail":"owner@example.com","countryCode":"US","hsaVersion":2,"isHideMyEmailSubscriptionActive":true},
			"hsaChallengeRequired":false,
			"hsaTrustedBrowser":true,
			"webservices":{"premiummailsettings":{"url":"https://p61-maildomainws.icloud.com:443","status":"active"}}
		}`, headers), nil

	case "/v2/hme/list":
		if request.URL.Host != "p61-maildomainws.icloud.com:443" || request.URL.Query().Get("dsid") != "123456789" || request.URL.Query().Get("clientId") == "" {
			return nil, errors.New("invalid HME list URL")
		}
		if !strings.Contains(request.Header.Get("Cookie"), "X-APPLE-WEBAUTH-TOKEN=web-token") {
			return nil, errors.New("persisted web auth cookie was not restored")
		}
		return testResponse(request, http.StatusOK, `{
			"success":true,
			"result":{"hmeEmails":[
				{"anonymousId":"one","hme":"one@icloud.com","isActive":true,"forwardToEmail":"owner@example.com","createTimestamp":1000},
				{"anonymousId":"two","hme":"two@icloud.com","isActive":false,"forwardToEmail":"owner@example.com","createTimestamp":2000}
			],"selectedForwardTo":"owner@example.com","forwardToEmails":["owner@example.com"]}
		}`, nil), nil
	default:
		return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL.String())
	}
}

func newFixtureClient(t *testing.T, fixture *authFixture) *Client {
	t.Helper()
	client, err := NewClient(Config{
		Transport: roundTripFunc(fixture.RoundTrip),
		Random:    deterministicRandom(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSignInVerifyAndListAliases(t *testing.T) {
	fixture := newAuthFixture(t, http.StatusConflict)
	client := newFixtureClient(t, fixture)
	session, needsTwoFactor, err := client.SignIn(context.Background(), "OWNER@example.com", fixture.password, RegionGlobal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !needsTwoFactor || session.SCNT != "scnt-two" || session.AuthAttributes != "attributes-one" {
		t.Fatalf("unexpected pending session: needs2FA=%v session=%#v", needsTwoFactor, session)
	}
	if len(session.Cookies) == 0 {
		t.Fatal("auth cookies were not persisted")
	}

	session, err = client.VerifyCode(context.Background(), session, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if session.DSID != "123456789" || session.TrustToken != "trust-token" || session.PremiumMailSettingsURL != "https://p61-maildomainws.icloud.com:443" {
		t.Fatalf("unexpected authenticated session: %#v", session)
	}
	if fixture.trustRequests != 1 || fixture.accountLoginRequests != 1 {
		t.Fatalf("trust/accountLogin requests = %d/%d, want 1/1", fixture.trustRequests, fixture.accountLoginRequests)
	}

	list, session, err := client.ListAliases(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Aliases) != 2 || !list.Aliases[0].IsActive || list.Aliases[1].IsActive || list.SelectedForwardTo != "owner@example.com" {
		t.Fatalf("unexpected list: %#v", list)
	}

	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(fixture.password)) || bytes.Contains(bytes.ToLower(encoded), []byte(`"password"`)) {
		t.Fatalf("session JSON contains password material: %s", encoded)
	}
	var restored Session
	if err := json.Unmarshal(encoded, &restored); err != nil || restored.DSID != session.DSID || len(restored.Cookies) == 0 {
		t.Fatalf("session JSON round trip failed: %#v, %v", restored, err)
	}
	for _, requestBody := range fixture.requestBodies {
		if strings.Contains(requestBody, fixture.password) {
			t.Fatal("plaintext password was sent over the SRP protocol")
		}
	}
}

func TestSignInWithoutChallengeSkipsTrust(t *testing.T) {
	fixture := newAuthFixture(t, http.StatusOK)
	client := newFixtureClient(t, fixture)
	session, needsTwoFactor, err := client.SignIn(context.Background(), fixture.appleID, fixture.password, RegionGlobal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if needsTwoFactor || session.DSID == "" {
		t.Fatalf("unexpected result: needs2FA=%v session=%#v", needsTwoFactor, session)
	}
	if fixture.trustRequests != 0 || fixture.accountLoginRequests != 1 {
		t.Fatalf("trust/accountLogin requests = %d/%d, want 0/1", fixture.trustRequests, fixture.accountLoginRequests)
	}
}

func TestVerifyCodeMapsHTTP400ToTypedCodeError(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusBadRequest, `{"errorCode":-21669,"errorMessage":"redacted"}`, nil), nil
	})
	client, err := NewClient(Config{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.VerifyCode(context.Background(), Session{Region: RegionGlobal}, "123456")
	if !errors.Is(err, ErrTwoFactorCode) {
		t.Fatalf("error = %v, want ErrTwoFactorCode", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode != 400 || typed.ServiceCode != "-21669" || strings.Contains(typed.Error(), "redacted") {
		t.Fatalf("typed error = %#v / %v", typed, err)
	}
}

func TestValidateReroutes421ToChinaSetup(t *testing.T) {
	var hosts []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		hosts = append(hosts, request.URL.Host)
		body, _ := io.ReadAll(request.Body)
		if string(body) != "null" {
			return nil, fmt.Errorf("validate body = %q, want null", body)
		}
		if len(hosts) == 1 {
			return testResponse(request, http.StatusMisdirectedRequest, `{"requestInfo":[{"country":"CN"}]}`, nil), nil
		}
		if request.Header.Get("Origin") != "https://www.icloud.com.cn" {
			return nil, errors.New("China retry did not change Origin")
		}
		return testResponse(request, http.StatusOK, `{"dsInfo":{"dsid":"42","primaryEmail":"owner@example.com","countryCode":"CN","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com.cn"}}}`, nil), nil
	})
	client, err := NewClient(Config{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.Validate(context.Background(), Session{Region: RegionGlobal})
	if err != nil {
		t.Fatal(err)
	}
	if session.Region != RegionChina || session.CountryCode != "CN" || len(hosts) != 2 || hosts[0] != "setup.icloud.com" || hosts[1] != "setup.icloud.com.cn" {
		t.Fatalf("reroute result = %#v, hosts=%v", session, hosts)
	}
}

func TestListAliasesRequiresCompleteUnpaginatedUniqueArray(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"success":true,"result":{}}`},
		{name: "null", body: `{"success":true,"result":{"hmeEmails":null}}`},
		{name: "object", body: `{"success":true,"result":{"hmeEmails":{}}}`},
		{name: "pagination", body: `{"success":true,"result":{"hmeEmails":[],"pagination":{"next":"token"}}}`},
		{name: "nested pagination object", body: `{"success":true,"result":{"hmeEmails":[],"metadata":{"nextCursor":"token"}}}`},
		{name: "nested pagination array", body: `{"success":true,"result":{"hmeEmails":[],"metadata":[{"continuation_token":"token"}]}}`},
		{name: "total mismatch", body: `{"success":true,"result":{"hmeEmails":[],"total":1}}`},
		{name: "total count mismatch", body: `{"success":true,"result":{"hmeEmails":[],"metadata":{"totalCount":1}}}`},
		{name: "total underscore mismatch", body: `{"success":true,"result":{"hmeEmails":[],"metadata":[{"total_count":1}]}}`},
		{name: "total wrong type", body: `{"success":true,"result":{"hmeEmails":[],"total":"0"}}`},
		{name: "alias not object", body: `{"success":true,"result":{"hmeEmails":["one@icloud.com"]}}`},
		{name: "missing hme", body: `{"success":true,"result":{"hmeEmails":[{"isActive":true,"forwardToEmail":"owner@example.com"}]}}`},
		{name: "hme wrong type", body: `{"success":true,"result":{"hmeEmails":[{"hme":1,"isActive":true,"forwardToEmail":"owner@example.com"}]}}`},
		{name: "missing active", body: `{"success":true,"result":{"hmeEmails":[{"hme":"one@icloud.com","forwardToEmail":"owner@example.com"}]}}`},
		{name: "active wrong type", body: `{"success":true,"result":{"hmeEmails":[{"hme":"one@icloud.com","isActive":"true","forwardToEmail":"owner@example.com"}]}}`},
		{name: "missing forward email", body: `{"success":true,"result":{"hmeEmails":[{"hme":"one@icloud.com","isActive":true}]}}`},
		{name: "forward email wrong type", body: `{"success":true,"result":{"hmeEmails":[{"hme":"one@icloud.com","isActive":true,"forwardToEmail":null}]}}`},
		{name: "empty forward email", body: `{"success":true,"result":{"hmeEmails":[{"hme":"one@icloud.com","isActive":false,"forwardToEmail":"  "}]}}`},
		{name: "duplicate address", body: `{"success":true,"result":{"hmeEmails":[{"anonymousId":"one","hme":"One@icloud.com","isActive":true,"forwardToEmail":"owner@example.com"},{"anonymousId":"two","hme":" one@icloud.com ","isActive":true,"forwardToEmail":"owner@example.com"}]}}`},
		{name: "duplicate anonymous ID", body: `{"success":true,"result":{"hmeEmails":[{"anonymousId":"same","hme":"one@icloud.com","isActive":true,"forwardToEmail":"owner@example.com"},{"anonymousId":"SAME","hme":"two@icloud.com","isActive":true,"forwardToEmail":"owner@example.com"}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return testResponse(request, http.StatusOK, test.body, nil), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = client.ListAliases(context.Background(), Session{
				Region:                 RegionGlobal,
				DSID:                   "42",
				ClientID:               "client-id",
				PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com",
			})
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestListAliasesAcceptsExplicitEmptyArray(t *testing.T) {
	client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusOK, `{"success":true,"result":{"hmeEmails":[],"forwardToEmails":[],"total":0,"metadata":{"totalCount":0},"summary":[{"total_count":0}]}}`, nil), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	list, _, err := client.ListAliases(context.Background(), Session{Region: RegionGlobal, DSID: "1", ClientID: "client", PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com"})
	if err != nil || list.Aliases == nil || len(list.Aliases) != 0 {
		t.Fatalf("list=%#v err=%v, want explicit empty list", list, err)
	}
}

func TestAppleURLAndResponseSecurityLimits(t *testing.T) {
	if _, err := NewClient(Config{Endpoints: map[Region]Endpoints{
		RegionGlobal: {Auth: "http://idmsa.apple.com/appleauth/auth", Home: "https://www.icloud.com", Setup: "https://setup.icloud.com/setup/ws/1"},
	}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("insecure endpoint error = %v", err)
	}

	called := false
	client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return testResponse(request, http.StatusOK, `{}`, nil), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.ListAliases(context.Background(), Session{Region: RegionGlobal, DSID: "1", ClientID: "client", PremiumMailSettingsURL: "https://icloud.com.attacker.example"})
	if !errors.Is(err, ErrInvalidSession) || called {
		t.Fatalf("dynamic endpoint error=%v called=%v", err, called)
	}

	client, err = NewClient(Config{
		MaxResponseBytes: 1024,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return testResponse(request, http.StatusOK, strings.Repeat("x", 1025), nil), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.ListAliases(context.Background(), Session{Region: RegionGlobal, DSID: "1", ClientID: "client", PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com"})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestRedirectCannotLeaveAppleHTTPSDomains(t *testing.T) {
	client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		headers := make(http.Header)
		headers.Set("Location", "https://attacker.example/stolen")
		return testResponse(request, http.StatusFound, "", headers), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.ListAliases(context.Background(), Session{Region: RegionGlobal, DSID: "1", ClientID: "client", PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com"})
	if !errors.Is(err, ErrService) {
		t.Fatalf("redirect error = %v, want ErrService", err)
	}
}

func TestCanceledContextIsPreserved(t *testing.T) {
	client, err := NewClient(Config{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = client.ListAliases(ctx, Session{Region: RegionGlobal, DSID: "1", ClientID: "client", PremiumMailSettingsURL: "https://p01-maildomainws.icloud.com"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestDefaultChinaEndpointsKeepGlobalIDMSA(t *testing.T) {
	endpoints, err := DefaultEndpoints(RegionChina)
	if err != nil {
		t.Fatal(err)
	}
	if endpoints.Auth != "https://idmsa.apple.com/appleauth/auth" || endpoints.Home != "https://www.icloud.com.cn" || endpoints.Setup != "https://setup.icloud.com.cn/setup/ws/1" {
		t.Fatalf("China endpoints = %#v", endpoints)
	}
	parsed, _ := url.Parse("https://evilicloud.com/v2/hme/list")
	if validICloudServiceURL(parsed) {
		t.Fatal("suffix-confusion host was accepted")
	}
}

func TestConfigBounds(t *testing.T) {
	for _, config := range []Config{
		{Timeout: time.Millisecond},
		{Timeout: 3 * time.Minute},
		{MaxResponseBytes: 100},
		{MaxResponseBytes: 65 << 20},
		{ClientBuildNumber: "bad value"},
	} {
		if _, err := NewClient(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %#v error = %v, want ErrInvalidConfig", config, err)
		}
	}
}
