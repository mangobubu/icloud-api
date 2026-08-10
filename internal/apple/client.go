package apple

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout          = 30 * time.Second
	defaultMaxResponseBytes = int64(8 << 20)
	userAgent               = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3.1 Safari/605.1.15"
)

// Config controls request limits and permits transport/endpoint injection.
// Injected endpoints still have to be HTTPS URLs on the expected Apple hosts;
// tests can intercept those requests with Transport.
type Config struct {
	Endpoints             map[Region]Endpoints
	Transport             http.RoundTripper
	Timeout               time.Duration
	MaxResponseBytes      int64
	Random                io.Reader
	ClientBuildNumber     string
	ClientMasteringNumber string
}

// Client is safe for concurrent use. Every operation gets an isolated cookie
// jar so sessions for different Apple IDs never share authentication state.
type Client struct {
	endpoints             map[Region]Endpoints
	transport             http.RoundTripper
	timeout               time.Duration
	maxResponseBytes      int64
	random                io.Reader
	randomMu              sync.Mutex
	clientBuildNumber     string
	clientMasteringNumber string
}

// NewClient validates all security-sensitive configuration up front.
func NewClient(config Config) (*Client, error) {
	endpoints := make(map[Region]Endpoints, 2)
	for _, region := range []Region{RegionGlobal, RegionChina} {
		defaults, err := DefaultEndpoints(region)
		if err != nil {
			return nil, err
		}
		endpoints[region] = defaults
	}
	for region, configured := range config.Endpoints {
		if region == "" {
			region = RegionGlobal
		}
		if region != RegionGlobal && region != RegionChina {
			return nil, fmt.Errorf("%w: endpoint region %q", ErrInvalidConfig, region)
		}
		endpoints[region] = configured
	}
	for region, configured := range endpoints {
		if err := validateEndpoints(region, configured); err != nil {
			return nil, err
		}
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < time.Second || timeout > 2*time.Minute {
		return nil, fmt.Errorf("%w: timeout must be between 1 second and 2 minutes", ErrInvalidConfig)
	}
	maxResponseBytes := config.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	if maxResponseBytes < 1024 || maxResponseBytes > 64<<20 {
		return nil, fmt.Errorf("%w: response limit must be between 1 KiB and 64 MiB", ErrInvalidConfig)
	}
	transport := config.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	build := strings.TrimSpace(config.ClientBuildNumber)
	if build == "" {
		build = defaultBuild
	}
	mastering := strings.TrimSpace(config.ClientMasteringNumber)
	if mastering == "" {
		mastering = defaultMastering
	}
	if !validBuildIdentifier(build) || !validBuildIdentifier(mastering) {
		return nil, fmt.Errorf("%w: invalid Apple build identifier", ErrInvalidConfig)
	}
	return &Client{
		endpoints:             endpoints,
		transport:             transport,
		timeout:               timeout,
		maxResponseBytes:      maxResponseBytes,
		random:                randomSource,
		clientBuildNumber:     build,
		clientMasteringNumber: mastering,
	}, nil
}

// SignIn performs Apple's current SRP login flow. The returned bool reports
// whether a two-factor code is required. The password is used only for local
// proof generation and is never copied into Session.
func (c *Client) SignIn(ctx context.Context, appleID, password string, region Region, previous *Session) (result Session, needsTwoFactor bool, err error) {
	if c == nil {
		return result, false, fmt.Errorf("%w: nil client", ErrInvalidConfig)
	}
	if region == "" {
		region = RegionGlobal
	}
	if _, ok := c.endpoints[region]; !ok {
		return result, false, fmt.Errorf("%w: unknown region %q", ErrInvalidConfig, region)
	}
	appleID = strings.ToLower(strings.TrimSpace(appleID))
	if appleID == "" || password == "" {
		return result, false, operationError("sign in", ErrAuthentication, 0, nil)
	}
	if previous != nil {
		result = *previous
		result.Cookies = append([]PersistentCookie(nil), previous.Cookies...)
		if result.Region != "" && result.Region != region {
			return result, false, fmt.Errorf("%w: session region mismatch", ErrInvalidSession)
		}
	}
	result.Region = region
	result.AppleID = appleID
	result.FrameID, err = c.newUUID()
	if err != nil {
		return result, false, operationError("sign in", ErrAuthentication, 0, err)
	}
	if result.ClientID == "" {
		result.ClientID, err = c.newUUID()
		if err != nil {
			return result, false, operationError("sign in", ErrAuthentication, 0, err)
		}
	}

	op, err := c.newOperation(&result)
	if err != nil {
		return result, false, err
	}
	defer op.persist(&result)

	if err := op.authorize(ctx); err != nil {
		return result, false, err
	}
	if err := op.federate(ctx, appleID); err != nil {
		return result, false, err
	}
	secret := make([]byte, 32)
	if err := c.readRandom(secret); err != nil {
		return result, false, operationError("initialize SRP", ErrAuthentication, 0, err)
	}
	srp, err := newSRPClient(bytes.NewReader(secret))
	if err != nil {
		return result, false, operationError("initialize SRP", ErrAuthentication, 0, err)
	}
	challenge, err := op.srpInit(ctx, appleID, srp.publicKey())
	if err != nil {
		return result, false, err
	}
	passwordKey, err := deriveApplePassword(password, challenge.salt, challenge.iterations, challenge.protocol)
	if err != nil {
		return result, false, operationError("derive SRP proof", ErrInvalidResponse, 0, err)
	}
	if err := srp.processChallenge([]byte(appleID), passwordKey, challenge.salt, challenge.serverPublic); err != nil {
		return result, false, operationError("derive SRP proof", ErrInvalidResponse, 0, err)
	}
	status, err := op.srpComplete(ctx, appleID, challenge.challenge, srp.m1, srp.m2)
	if err != nil {
		return result, false, err
	}
	switch status {
	case http.StatusConflict:
		// Newer iOS versions need an explicit trigger. Some account variants
		// answer 405 here even though a device code is already usable.
		_, _ = op.requestTrustedDeviceCode(ctx)
		return result, true, nil
	case http.StatusOK:
		account, err := op.accountLogin(ctx)
		if err != nil {
			return result, false, err
		}
		result.applyAccount(account)
		result.ValidatedAt = time.Now().UTC()
		needsTwoFactor = requiresTwoFactor(result)
		return result, needsTwoFactor, nil
	default:
		return result, false, operationError("complete SRP sign in", ErrInvalidResponse, status, nil)
	}
}

// RequestCode best-effort triggers a code on trusted Apple devices.
func (c *Client) RequestCode(ctx context.Context, session Session) (result Session, err error) {
	result = session
	op, err := c.newOperation(&result)
	if err != nil {
		return result, err
	}
	defer op.persist(&result)
	status, err := op.requestTrustedDeviceCode(ctx)
	if err != nil {
		return result, err
	}
	if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusMethodNotAllowed {
		return result, operationError("request two-factor code", ErrService, status, nil)
	}
	return result, nil
}

// VerifyCode verifies a trusted-device code, trusts the browser, and exchanges
// the IDMSA token for an iCloud web session.
func (c *Client) VerifyCode(ctx context.Context, session Session, code string) (result Session, err error) {
	result = session
	if !validSecurityCode(code) {
		return result, operationError("verify two-factor code", ErrTwoFactorCode, 0, nil)
	}
	op, err := c.newOperation(&result)
	if err != nil {
		return result, err
	}
	defer op.persist(&result)
	response, err := op.request(ctx, "verify two-factor code", http.MethodPost, op.endpoints.Auth+"/verify/trusteddevice/securitycode", map[string]any{
		"securityCode": map[string]string{"code": code},
	}, op.authHeaders())
	if err != nil {
		return result, err
	}
	acceptedConflict := response.status == http.StatusConflict && response.header.Get("X-Apple-Session-Token") != ""
	if response.status != http.StatusOK && response.status != http.StatusNoContent && !acceptedConflict {
		kind := ErrService
		if response.status == http.StatusBadRequest || response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == http.StatusConflict || response.status == http.StatusUnprocessableEntity {
			kind = ErrTwoFactorCode
		}
		return result, &Error{
			Op:          "verify two-factor code",
			Kind:        kind,
			StatusCode:  response.status,
			ServiceCode: responseServiceCode(response.body),
			Retryable:   response.status == http.StatusTooManyRequests || response.status >= 500,
		}
	}
	if err := op.trust(ctx); err != nil {
		return result, err
	}
	account, err := op.accountLogin(ctx)
	if err != nil {
		return result, err
	}
	result.applyAccount(account)
	result.ValidatedAt = time.Now().UTC()
	if requiresTwoFactor(result) {
		return result, operationError("complete two-factor authentication", ErrTwoFactorCode, 0, nil)
	}
	return result, nil
}

// Validate checks a persisted web session without triggering a fresh sign-in.
func (c *Client) Validate(ctx context.Context, session Session) (result Session, err error) {
	result = session
	op, err := c.newOperation(&result)
	if err != nil {
		return result, err
	}
	defer op.persist(&result)
	account, err := op.validate(ctx)
	if err != nil {
		return result, err
	}
	result.applyAccount(account)
	result.ValidatedAt = time.Now().UTC()
	return result, nil
}

// ListAliases returns Apple's authoritative full Hide My Email list and the
// updated session (including any rotated cookies).
func (c *Client) ListAliases(ctx context.Context, session Session) (list ListResult, result Session, err error) {
	result = session
	op, err := c.newOperation(&result)
	if err != nil {
		return list, result, err
	}
	defer op.persist(&result)
	if result.PremiumMailSettingsURL == "" || result.DSID == "" {
		return list, result, operationError("list Hide My Email aliases", ErrInvalidSession, 0, nil)
	}
	requestURL, err := c.premiumMailSettingsRequestURL(result, "/v2/hme/list")
	if err != nil {
		return list, result, fmt.Errorf("%w: invalid premium mail service URL", ErrInvalidSession)
	}
	headers := op.serviceHeaders()
	headers.Set("Accept", "*/*")
	headers.Set("Content-Type", "text/plain")
	response, err := op.request(ctx, "list Hide My Email aliases", http.MethodGet, requestURL, nil, headers)
	if err != nil {
		return list, result, err
	}
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == 450 {
		return list, result, responseError("list Hide My Email aliases", ErrInvalidSession, response)
	}
	if response.status == http.StatusPreconditionFailed {
		return list, result, responseError("list Hide My Email aliases", ErrTermsRequired, response)
	}
	if response.status < 200 || response.status >= 300 {
		return list, result, responseError("list Hide My Email aliases", ErrService, response)
	}
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, err)
	}
	if !envelope.Success {
		return list, result, responseError("list Hide My Email aliases", ErrService, response)
	}
	if containsPaginationMarker(response.body) || containsPaginationMarker(envelope.Result) {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("unexpected pagination marker"))
	}
	var resultFields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Result, &resultFields); err != nil {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, err)
	}
	rawAliases, present := resultFields["hmeEmails"]
	trimmedAliases := bytes.TrimSpace(rawAliases)
	if !present || len(trimmedAliases) == 0 || trimmedAliases[0] != '[' {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("hmeEmails must be an array"))
	}
	var rawAliasEntries []json.RawMessage
	if err := json.Unmarshal(rawAliases, &rawAliasEntries); err != nil {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("hmeEmails must be an array"))
	}
	if err := validateListCounts(response.body, len(rawAliasEntries)); err != nil {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, err)
	}
	for index, rawAlias := range rawAliasEntries {
		if err := validateAliasFields(rawAlias); err != nil {
			return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, fmt.Errorf("hmeEmails[%d]: %w", index, err))
		}
	}
	if err := json.Unmarshal(envelope.Result, &list); err != nil {
		return list, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, err)
	}
	if list.Aliases == nil {
		list.Aliases = []Alias{}
	}
	seen := make(map[string]struct{}, len(list.Aliases))
	seenAnonymousIDs := make(map[string]struct{}, len(list.Aliases))
	for _, alias := range list.Aliases {
		address := strings.ToLower(strings.TrimSpace(alias.HME))
		if address == "" {
			return ListResult{}, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("empty alias address"))
		}
		if _, duplicate := seen[address]; duplicate {
			return ListResult{}, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("duplicate alias address"))
		}
		seen[address] = struct{}{}
		anonymousID := strings.ToLower(strings.TrimSpace(alias.AnonymousID))
		if anonymousID != "" {
			if _, duplicate := seenAnonymousIDs[anonymousID]; duplicate {
				return ListResult{}, result, operationError("decode Hide My Email list", ErrInvalidResponse, response.status, errors.New("duplicate anonymous ID"))
			}
			seenAnonymousIDs[anonymousID] = struct{}{}
		}
	}
	return list, result, nil
}

// UpdateForwardTo sets the account-wide Hide My Email forwarding target. The
// caller must verify the selected target with ListAliases before reserving an
// alias because a lost response can leave the mutation outcome uncertain.
func (c *Client) UpdateForwardTo(ctx context.Context, session Session, forwardToEmail string) (result Session, err error) {
	result = session
	forwardToEmail, err = normalizeHMEAddress(forwardToEmail)
	if err != nil {
		return result, operationError("update Hide My Email forwarding target", ErrInvalidConfig, 0,
			errors.New("invalid forwarding email address"))
	}
	op, err := c.newOperation(&result)
	if err != nil {
		return result, err
	}
	defer op.persist(&result)
	if result.PremiumMailSettingsURL == "" || result.DSID == "" {
		return result, operationError("update Hide My Email forwarding target", ErrInvalidSession, 0, nil)
	}
	requestURL, err := c.premiumMailSettingsRequestURL(result, "/v1/hme/updateForwardTo")
	if err != nil {
		return result, fmt.Errorf("%w: invalid premium mail service URL", ErrInvalidSession)
	}
	response, err := op.request(
		ctx,
		"update Hide My Email forwarding target",
		http.MethodPost,
		requestURL,
		map[string]string{"forwardToEmail": forwardToEmail},
		op.serviceHeaders(),
	)
	if err != nil {
		return result, err
	}
	if err := decodeHMEMutation("update Hide My Email forwarding target", response); err != nil {
		return result, err
	}
	return result, nil
}

// DeactivateAlias stops delivery to one Hide My Email address. Apple requires
// the opaque anonymous ID returned by ListAliases rather than the address.
func (c *Client) DeactivateAlias(ctx context.Context, session Session, anonymousID string) (Session, error) {
	return c.mutateAlias(ctx, session, anonymousID, "/v1/hme/deactivate", "deactivate Hide My Email alias")
}

// DeleteAlias permanently removes one deactivated Hide My Email address.
// Callers are responsible for deactivating active aliases first.
func (c *Client) DeleteAlias(ctx context.Context, session Session, anonymousID string) (Session, error) {
	return c.mutateAlias(ctx, session, anonymousID, "/v1/hme/delete", "delete Hide My Email alias")
}

func (c *Client) mutateAlias(
	ctx context.Context,
	session Session,
	anonymousID string,
	endpoint string,
	operation string,
) (result Session, err error) {
	result = session
	if !validHMEAnonymousID(anonymousID) {
		return result, operationError(operation, ErrInvalidConfig, 0,
			errors.New("invalid Hide My Email anonymous ID"))
	}
	op, err := c.newOperation(&result)
	if err != nil {
		return result, err
	}
	defer op.persist(&result)
	if result.PremiumMailSettingsURL == "" || result.DSID == "" {
		return result, operationError(operation, ErrInvalidSession, 0, nil)
	}
	requestURL, err := c.premiumMailSettingsRequestURL(result, endpoint)
	if err != nil {
		return result, fmt.Errorf("%w: invalid premium mail service URL", ErrInvalidSession)
	}
	response, err := op.request(
		ctx,
		operation,
		http.MethodPost,
		requestURL,
		map[string]string{"anonymousId": anonymousID},
		op.serviceHeaders(),
	)
	if err != nil {
		return result, nonRetryableAppleError(err)
	}
	if err := decodeHMEMutation(operation, response); err != nil {
		return result, nonRetryableAppleError(err)
	}
	return result, nil
}

// CreateAlias generates and reserves one new Hide My Email address. The
// reserve request is a remote side effect, so errors after it starts are
// deliberately marked non-retryable even when Apple returns a transient HTTP
// status or the response is lost. When the reserve outcome is ambiguous,
// created carries the generated address and requested metadata for read-only
// reconciliation by the caller.
func (c *Client) CreateAlias(ctx context.Context, session Session, label, note string) (created Alias, result Session, err error) {
	result = session
	op, err := c.newOperation(&result)
	if err != nil {
		return created, result, err
	}
	defer op.persist(&result)
	if result.PremiumMailSettingsURL == "" || result.DSID == "" {
		return created, result, operationError("create Hide My Email alias", ErrInvalidSession, 0, nil)
	}

	generateURL, err := c.premiumMailSettingsRequestURL(result, "/v1/hme/generate")
	if err != nil {
		return created, result, fmt.Errorf("%w: invalid premium mail service URL", ErrInvalidSession)
	}
	language := "en-us"
	if result.Region == RegionChina {
		language = "zh-cn"
	}
	response, err := op.request(
		ctx,
		"generate Hide My Email alias",
		http.MethodPost,
		generateURL,
		map[string]string{"langCode": language},
		op.serviceHeaders(),
	)
	if err != nil {
		return created, result, err
	}
	generatedResult, err := decodeHMEResult("generate Hide My Email alias", response)
	if err != nil {
		return created, result, err
	}
	candidate, err := decodeGeneratedHME(generatedResult)
	if err != nil {
		return created, result, operationError("decode generated Hide My Email alias", ErrInvalidResponse, response.status, err)
	}

	reserveURL, err := c.premiumMailSettingsRequestURL(result, "/v1/hme/reserve")
	if err != nil {
		return created, result, fmt.Errorf("%w: invalid premium mail service URL", ErrInvalidSession)
	}
	response, err = op.request(
		ctx,
		"reserve Hide My Email alias",
		http.MethodPost,
		reserveURL,
		map[string]string{"hme": candidate, "label": label, "note": note},
		op.serviceHeaders(),
	)
	if err != nil {
		err = nonRetryableAppleError(err)
		if uncertainReserveError(err) {
			return reserveCandidate(candidate, label, note), result, err
		}
		return Alias{}, result, err
	}
	reservedResult, err := decodeHMEResult("reserve Hide My Email alias", response)
	if err != nil {
		err = nonRetryableAppleError(err)
		if !responseExplicitlyFailed(response.body) && uncertainReserveError(err) {
			return reserveCandidate(candidate, label, note), result, err
		}
		return Alias{}, result, err
	}
	created, err = decodeReservedAlias(reservedResult, candidate)
	if err != nil {
		err = operationError("decode reserved Hide My Email alias", ErrInvalidResponse, response.status, err)
		return reserveCandidate(candidate, label, note), result, nonRetryableAppleError(err)
	}
	return created, result, nil
}

func reserveCandidate(hme, label, note string) Alias {
	return Alias{HME: hme, Label: label, Note: note}
}

func uncertainReserveError(err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return false
	}
	if typed.StatusCode == 0 {
		return errors.Is(typed, ErrService) && typed.Err != nil
	}
	return (typed.StatusCode > 0 && typed.StatusCode < 200) ||
		typed.StatusCode >= 300 && typed.StatusCode < 400 ||
		typed.StatusCode >= 500 ||
		(typed.StatusCode >= 200 && typed.StatusCode < 300 &&
			(errors.Is(typed, ErrInvalidResponse) || errors.Is(typed, ErrResponseTooLarge)))
}

func responseExplicitlyFailed(body []byte) bool {
	var envelope struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Success != nil && !*envelope.Success
}

func (c *Client) premiumMailSettingsRequestURL(session Session, endpoint string) (string, error) {
	base, err := url.Parse(session.PremiumMailSettingsURL)
	if err != nil || !validICloudServiceURL(base) {
		return "", errors.New("invalid premium mail service URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + endpoint
	query := base.Query()
	query.Set("clientBuildNumber", c.clientBuildNumber)
	query.Set("clientMasteringNumber", c.clientMasteringNumber)
	query.Set("clientId", session.ClientID)
	query.Set("dsid", session.DSID)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func decodeHMEResult(operation string, response responseData) (json.RawMessage, error) {
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == 450 {
		return nil, responseError(operation, ErrInvalidSession, response)
	}
	if response.status == http.StatusPreconditionFailed {
		return nil, responseError(operation, ErrTermsRequired, response)
	}
	if response.status < 200 || response.status >= 300 {
		return nil, responseError(operation, ErrService, response)
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil {
		return nil, operationError("decode "+operation, ErrInvalidResponse, response.status, err)
	}
	if envelope.Success == nil {
		return nil, operationError("decode "+operation, ErrInvalidResponse, response.status, errors.New("success must be a boolean"))
	}
	if !*envelope.Success {
		return nil, responseError(operation, ErrService, response)
	}
	trimmed := bytes.TrimSpace(envelope.Result)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, operationError("decode "+operation, ErrInvalidResponse, response.status, errors.New("result is required"))
	}
	return envelope.Result, nil
}

func decodeHMEMutation(operation string, response responseData) error {
	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == 450 {
		return responseError(operation, ErrInvalidSession, response)
	}
	if response.status == http.StatusPreconditionFailed {
		return responseError(operation, ErrTermsRequired, response)
	}
	if response.status < 200 || response.status >= 300 {
		return responseError(operation, ErrService, response)
	}
	var envelope struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil {
		return operationError("decode "+operation, ErrInvalidResponse, response.status, err)
	}
	if envelope.Success == nil {
		return operationError("decode "+operation, ErrInvalidResponse, response.status,
			errors.New("success must be a boolean"))
	}
	if !*envelope.Success {
		return responseError(operation, ErrService, response)
	}
	return nil
}

func decodeGeneratedHME(result json.RawMessage) (string, error) {
	fields, err := decodeJSONObject(result, "generate result")
	if err != nil {
		return "", err
	}
	rawHME, present := fields["hme"]
	if !present {
		return "", errors.New("generate result.hme is required")
	}
	trimmed := bytes.TrimSpace(rawHME)
	if len(trimmed) == 0 {
		return "", errors.New("generate result.hme is required")
	}
	if trimmed[0] == '{' {
		nested, err := decodeJSONObject(rawHME, "generate result.hme")
		if err != nil {
			return "", err
		}
		rawHME, present = nested["hme"]
		if !present {
			return "", errors.New("generate result.hme.hme is required")
		}
	}
	address, err := decodeJSONString(rawHME, "generate result.hme")
	if err != nil {
		return "", err
	}
	if _, err := normalizeHMEAddress(address); err != nil {
		return "", fmt.Errorf("generate result.hme: %w", err)
	}
	return strings.TrimSpace(address), nil
}

func decodeReservedAlias(result json.RawMessage, candidate string) (Alias, error) {
	fields, err := decodeJSONObject(result, "reserve result")
	if err != nil {
		return Alias{}, err
	}
	rawAlias, present := fields["hme"]
	if !present {
		return Alias{}, errors.New("reserve result.hme is required")
	}
	aliasFields, err := decodeJSONObject(rawAlias, "reserve result.hme")
	if err != nil {
		return Alias{}, err
	}
	rawAddress, present := aliasFields["hme"]
	if !present {
		return Alias{}, errors.New("reserve result.hme.hme is required")
	}
	address, err := decodeJSONString(rawAddress, "reserve result.hme.hme")
	if err != nil {
		return Alias{}, err
	}
	normalizedAddress, err := normalizeHMEAddress(address)
	if err != nil {
		return Alias{}, fmt.Errorf("reserve result.hme.hme: %w", err)
	}
	normalizedCandidate, err := normalizeHMEAddress(candidate)
	if err != nil {
		return Alias{}, fmt.Errorf("generated candidate: %w", err)
	}
	if normalizedAddress != normalizedCandidate {
		return Alias{}, errors.New("reserved Hide My Email alias does not match generated candidate")
	}
	var reserved Alias
	if err := json.Unmarshal(rawAlias, &reserved); err != nil {
		return Alias{}, fmt.Errorf("decode reserve result.hme: %w", err)
	}
	// encoding/json matches struct fields case-insensitively. Pin the address to
	// the exact lower-case JSON member validated above so a second HME variant
	// cannot overwrite it after the candidate comparison.
	reserved.HME = strings.TrimSpace(address)
	// A successful reserve response is not guaranteed to repeat every field
	// returned by the authoritative list endpoint. In particular, Apple may
	// omit isActive; reserve success itself means the new alias is active. Keep
	// an explicit false value intact so callers can still reject contradictory
	// responses.
	if _, present := aliasFields["isActive"]; !present {
		reserved.IsActive = true
	}
	return reserved, nil
}

func decodeJSONObject(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%s must be an object: %w", name, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return fields, nil
}

func decodeJSONString(raw json.RawMessage, name string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("%s must be a string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", name)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	return value, nil
}

func normalizeHMEAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if value == "" || err != nil || parsed.Name != "" || !strings.EqualFold(strings.TrimSpace(parsed.Address), value) {
		return "", errors.New("must be a valid email address")
	}
	return strings.ToLower(value), nil
}

func nonRetryableAppleError(err error) error {
	var typed *Error
	if !errors.As(err, &typed) {
		return err
	}
	copy := *typed
	copy.Retryable = false
	return &copy
}

type operation struct {
	owner     *Client
	session   *Session
	jar       *PersistentJar
	http      *http.Client
	endpoints Endpoints
}

type responseData struct {
	status int
	header http.Header
	body   []byte
}

func (c *Client) newOperation(session *Session) (*operation, error) {
	if c == nil || session == nil {
		return nil, fmt.Errorf("%w: nil client or session", ErrInvalidConfig)
	}
	region := session.Region
	if region == "" {
		region = RegionGlobal
		session.Region = region
	}
	endpoints, ok := c.endpoints[region]
	if !ok {
		return nil, fmt.Errorf("%w: unknown session region %q", ErrInvalidSession, region)
	}
	for _, cookie := range session.Cookies {
		if !isAppleDomain(strings.TrimPrefix(cookie.Domain, ".")) {
			return nil, fmt.Errorf("%w: non-Apple cookie domain", ErrInvalidSession)
		}
	}
	jar, err := NewPersistentJar(session.Cookies)
	if err != nil {
		return nil, err
	}
	op := &operation{owner: c, session: session, jar: jar, endpoints: endpoints}
	op.http = &http.Client{
		Transport: c.transport,
		Jar:       jar,
		Timeout:   c.timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many Apple redirects")
			}
			if !validAppleURL(request.URL) {
				return errors.New("Apple redirect left the allowed HTTPS domains")
			}
			return nil
		},
	}
	return op, nil
}

func (op *operation) persist(session *Session) {
	session.Cookies = op.jar.Export()
}

func (op *operation) authorize(ctx context.Context) error {
	frame := "auth-" + op.session.FrameID
	u, _ := url.Parse(op.endpoints.Auth + "/authorize/signin")
	query := u.Query()
	query.Set("frame_id", frame)
	query.Set("language", "en_US")
	query.Set("skVersion", "7")
	query.Set("iframeId", frame)
	query.Set("client_id", defaultWidgetKey)
	query.Set("redirect_uri", op.endpoints.Home)
	query.Set("response_type", "code")
	query.Set("response_mode", "web_message")
	query.Set("state", frame)
	query.Set("authVersion", "latest")
	u.RawQuery = query.Encode()
	headers := op.authHeaders()
	headers.Set("Accept", "*/*")
	response, err := op.request(ctx, "initialize sign in", http.MethodGet, u.String(), nil, headers)
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return operationError("initialize sign in", ErrAuthentication, response.status, nil)
	}
	return nil
}

func (op *operation) federate(ctx context.Context, appleID string) error {
	response, err := op.request(ctx, "federate Apple ID", http.MethodPost, op.endpoints.Auth+"/federate?isRememberMeEnabled=true", map[string]any{
		"accountName": appleID,
		"rememberMe":  true,
	}, op.authHeaders())
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		kind := ErrService
		if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
			kind = ErrAuthentication
		}
		return operationError("federate Apple ID", kind, response.status, nil)
	}
	return nil
}

type srpChallenge struct {
	salt         []byte
	serverPublic []byte
	challenge    string
	iterations   int
	protocol     string
}

func (op *operation) srpInit(ctx context.Context, appleID string, public []byte) (srpChallenge, error) {
	response, err := op.request(ctx, "initialize SRP", http.MethodPost, op.endpoints.Auth+"/signin/init", map[string]any{
		"a":           base64.StdEncoding.EncodeToString(public),
		"accountName": appleID,
		"protocols":   []string{"s2k", "s2k_fo"},
	}, op.authHeaders())
	if err != nil {
		return srpChallenge{}, err
	}
	if response.status != http.StatusOK {
		kind := ErrService
		if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
			kind = ErrAuthentication
		}
		return srpChallenge{}, operationError("initialize SRP", kind, response.status, nil)
	}
	var body struct {
		Salt      string `json:"salt"`
		B         string `json:"b"`
		C         string `json:"c"`
		Iteration int    `json:"iteration"`
		Protocol  string `json:"protocol"`
	}
	if err := json.Unmarshal(response.body, &body); err != nil {
		return srpChallenge{}, operationError("decode SRP challenge", ErrInvalidResponse, response.status, err)
	}
	salt, err := base64.StdEncoding.Strict().DecodeString(body.Salt)
	if err != nil || len(salt) == 0 || len(salt) > 1024 {
		return srpChallenge{}, operationError("decode SRP salt", ErrInvalidResponse, response.status, err)
	}
	serverPublic, err := base64.StdEncoding.Strict().DecodeString(body.B)
	if err != nil || len(serverPublic) == 0 || len(serverPublic) > appleSRPSize {
		return srpChallenge{}, operationError("decode SRP public value", ErrInvalidResponse, response.status, err)
	}
	if body.C == "" {
		return srpChallenge{}, operationError("decode SRP challenge", ErrInvalidResponse, response.status, nil)
	}
	return srpChallenge{salt: salt, serverPublic: serverPublic, challenge: body.C, iterations: body.Iteration, protocol: body.Protocol}, nil
}

func (op *operation) srpComplete(ctx context.Context, appleID, challenge string, m1, m2 []byte) (int, error) {
	trustTokens := []string{}
	if op.session.TrustToken != "" {
		trustTokens = append(trustTokens, op.session.TrustToken)
	}
	response, err := op.request(ctx, "complete SRP sign in", http.MethodPost, op.endpoints.Auth+"/signin/complete?isRememberMeEnabled=true", map[string]any{
		"accountName": appleID,
		"c":           challenge,
		"m1":          base64.StdEncoding.EncodeToString(m1),
		"m2":          base64.StdEncoding.EncodeToString(m2),
		"rememberMe":  true,
		"trustTokens": trustTokens,
	}, op.authHeaders())
	if err != nil {
		return 0, err
	}
	switch response.status {
	case http.StatusOK, http.StatusConflict:
		return response.status, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, operationError("complete SRP sign in", ErrAuthentication, response.status, nil)
	case http.StatusPreconditionFailed:
		return 0, operationError("complete SRP sign in", ErrTermsRequired, response.status, nil)
	default:
		return 0, operationError("complete SRP sign in", ErrService, response.status, nil)
	}
}

func (op *operation) requestTrustedDeviceCode(ctx context.Context) (int, error) {
	response, err := op.request(ctx, "request two-factor code", http.MethodPut, op.endpoints.Auth+"/verify/trusteddevice/securitycode", nil, op.authHeaders())
	if err != nil {
		return 0, err
	}
	return response.status, nil
}

func (op *operation) trust(ctx context.Context) error {
	response, err := op.request(ctx, "trust Apple session", http.MethodGet, op.endpoints.Auth+"/2sv/trust", nil, op.authHeaders())
	if err != nil {
		return err
	}
	if response.status != http.StatusOK && response.status != http.StatusNoContent {
		return operationError("trust Apple session", ErrService, response.status, nil)
	}
	return nil
}

func (op *operation) accountLogin(ctx context.Context) (accountResponse, error) {
	if op.session.SessionToken == "" {
		return accountResponse{}, operationError("exchange Apple session token", ErrInvalidSession, 0, nil)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response, err := op.request(ctx, "exchange Apple session token", http.MethodPost, op.endpoints.Setup+"/accountLogin", map[string]any{
			"accountCountryCode": op.session.CountryCode,
			"dsWebAuthToken":     op.session.SessionToken,
			"extended_login":     true,
			"trustToken":         op.session.TrustToken,
		}, op.serviceHeaders())
		if err != nil {
			return accountResponse{}, err
		}
		if response.status == http.StatusMisdirectedRequest && attempt == 0 && responseCountry(response.body) == "CN" {
			op.setRegion(RegionChina)
			continue
		}
		if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == 450 {
			return accountResponse{}, operationError("exchange Apple session token", ErrInvalidSession, response.status, nil)
		}
		if response.status < 200 || response.status >= 300 {
			return accountResponse{}, operationError("exchange Apple session token", ErrService, response.status, nil)
		}
		account, err := decodeAccountResponse(response.body)
		if err != nil {
			return accountResponse{}, operationError("decode Apple account", ErrInvalidResponse, response.status, err)
		}
		return account, nil
	}
	return accountResponse{}, operationError("exchange Apple session token", ErrInvalidSession, http.StatusMisdirectedRequest, nil)
}

func (op *operation) validate(ctx context.Context) (accountResponse, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := op.requestRaw(ctx, "validate Apple session", http.MethodPost, op.endpoints.Setup+"/validate", []byte("null"), op.serviceHeaders())
		if err != nil {
			return accountResponse{}, err
		}
		if response.status == http.StatusMisdirectedRequest && attempt == 0 && responseCountry(response.body) == "CN" {
			op.setRegion(RegionChina)
			continue
		}
		if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden || response.status == 450 || response.status == http.StatusMisdirectedRequest {
			return accountResponse{}, operationError("validate Apple session", ErrInvalidSession, response.status, nil)
		}
		if response.status < 200 || response.status >= 300 {
			return accountResponse{}, operationError("validate Apple session", ErrService, response.status, nil)
		}
		account, err := decodeAccountResponse(response.body)
		if err != nil {
			return accountResponse{}, operationError("decode Apple session", ErrInvalidResponse, response.status, err)
		}
		if account.Success != nil && !*account.Success && string(account.DSInfo.DSID) == "" {
			return accountResponse{}, operationError("validate Apple session", ErrInvalidSession, response.status, nil)
		}
		return account, nil
	}
	return accountResponse{}, operationError("validate Apple session", ErrInvalidSession, http.StatusMisdirectedRequest, nil)
}

func (op *operation) setRegion(region Region) {
	op.session.Region = region
	op.session.CountryCode = "CN"
	op.endpoints = op.owner.endpoints[region]
}

func (op *operation) authHeaders() http.Header {
	frame := "auth-" + op.session.FrameID
	originURL, _ := url.Parse(op.endpoints.Auth)
	origin := "https://" + originURL.Host
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Origin", origin)
	headers.Set("Referer", origin+"/")
	headers.Set("User-Agent", userAgent)
	headers.Set("X-Apple-Widget-Key", defaultWidgetKey)
	headers.Set("X-Apple-OAuth-Client-Id", defaultWidgetKey)
	headers.Set("X-Apple-OAuth-Client-Type", "firstPartyAuth")
	headers.Set("X-Apple-OAuth-Redirect-URI", op.endpoints.Home)
	headers.Set("X-Apple-OAuth-Require-Grant-Code", "true")
	headers.Set("X-Apple-OAuth-Response-Mode", "web_message")
	headers.Set("X-Apple-OAuth-Response-Type", "code")
	headers.Set("X-Apple-OAuth-State", frame)
	headers.Set("X-Apple-Frame-Id", frame)
	headers.Set("X-Requested-With", "XMLHttpRequest")
	headers.Set("X-Apple-Mandate-Security-Upgrade", "0")
	headers.Set("X-Apple-I-Require-UE", "true")
	headers.Set("X-Apple-I-FD-Client-Info", `{"U":"`+userAgent+`","L":"en-US","Z":"GMT+00:00","V":"1.1","F":""}`)
	if op.session.SCNT != "" {
		headers.Set("scnt", op.session.SCNT)
	}
	if op.session.SessionID != "" {
		headers.Set("X-Apple-ID-Session-Id", op.session.SessionID)
	}
	if op.session.AuthAttributes != "" {
		headers.Set("X-Apple-Auth-Attributes", op.session.AuthAttributes)
	}
	return headers
}

func (op *operation) serviceHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("Accept", "application/json")
	headers.Set("Content-Type", "application/json")
	headers.Set("Origin", op.endpoints.Home)
	headers.Set("Referer", op.endpoints.Home+"/")
	headers.Set("User-Agent", userAgent)
	return headers
}

func (op *operation) request(ctx context.Context, operation, method, rawURL string, body any, headers http.Header) (responseData, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return responseData{}, operationError(operation, ErrInvalidConfig, 0, err)
		}
	}
	return op.requestRaw(ctx, operation, method, rawURL, encoded, headers)
}

func (op *operation) requestRaw(ctx context.Context, operation, method, rawURL string, body []byte, headers http.Header) (responseData, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !validAppleURL(u) {
		return responseData{}, operationError(operation, ErrInvalidConfig, 0, err)
	}
	requestContext, cancel := context.WithTimeout(ctx, op.owner.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestContext, method, u.String(), reader)
	if err != nil {
		return responseData{}, operationError(operation, ErrInvalidConfig, 0, err)
	}
	request.Header = headers.Clone()
	response, err := op.http.Do(request)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			op.capture(response.Header)
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		if requestContext.Err() != nil {
			err = requestContext.Err()
		}
		return responseData{}, operationError(operation, ErrService, status, err)
	}
	if response.Body == nil {
		return responseData{}, operationError(operation, ErrInvalidResponse, response.StatusCode, errors.New("empty response body"))
	}
	defer response.Body.Close()
	op.capture(response.Header)
	limited := io.LimitReader(response.Body, op.owner.maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return responseData{}, operationError(operation, ErrInvalidResponse, response.StatusCode, err)
	}
	if int64(len(responseBody)) > op.owner.maxResponseBytes {
		return responseData{}, operationError(operation, ErrResponseTooLarge, response.StatusCode, nil)
	}
	return responseData{status: response.StatusCode, header: response.Header.Clone(), body: responseBody}, nil
}

func (op *operation) capture(headers http.Header) {
	if value := headers.Get("X-Apple-ID-Account-Country"); value != "" {
		op.session.CountryCode = value
	}
	if value := headers.Get("X-Apple-ID-Session-Id"); value != "" {
		op.session.SessionID = value
	}
	if value := headers.Get("X-Apple-Session-Token"); value != "" {
		op.session.SessionToken = value
	}
	if value := headers.Get("X-Apple-TwoSV-Trust-Token"); value != "" {
		op.session.TrustToken = value
	}
	if value := headers.Get("scnt"); value != "" {
		op.session.SCNT = value
	}
	if value := headers.Get("X-Apple-Auth-Attributes"); value != "" {
		op.session.AuthAttributes = value
	}
}

func decodeAccountResponse(body []byte) (accountResponse, error) {
	var response accountResponse
	if len(body) == 0 {
		return response, errors.New("empty account response")
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return response, err
	}
	if string(response.DSInfo.DSID) == "" && len(response.Webservices) == 0 {
		return response, errors.New("account response contains no identity or services")
	}
	return response, nil
}

func responseCountry(body []byte) string {
	var response struct {
		RequestInfo []struct {
			Country string `json:"country"`
		} `json:"requestInfo"`
	}
	if json.Unmarshal(body, &response) == nil && len(response.RequestInfo) != 0 {
		return strings.ToUpper(response.RequestInfo[0].Country)
	}
	return ""
}

func responseServiceCode(body []byte) string {
	var response struct {
		ErrorCode       json.RawMessage `json:"errorCode"`
		ServerErrorCode json.RawMessage `json:"serverErrorCode"`
		Error           *struct {
			ErrorCode json.RawMessage `json:"errorCode"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) != nil {
		return ""
	}
	if code := serviceCodeValue(response.ErrorCode); code != "" {
		return code
	}
	if code := serviceCodeValue(response.ServerErrorCode); code != "" {
		return code
	}
	if response.Error != nil {
		return serviceCodeValue(response.Error.ErrorCode)
	}
	return ""
}

func serviceCodeValue(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) == nil {
			return strings.TrimSpace(value)
		}
	}
	return string(trimmed)
}

func responseError(operation string, kind error, response responseData) error {
	return &Error{
		Op:          operation,
		Kind:        kind,
		StatusCode:  response.status,
		ServiceCode: responseServiceCode(response.body),
		Retryable:   retryableStatus(response.status),
	}
}

func containsPaginationMarker(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return false
	}
	return hasPaginationMarker(value)
}

func hasPaginationMarker(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isPaginationKey(key) || hasPaginationMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasPaginationMarker(child) {
				return true
			}
		}
	}
	return false
}

func isPaginationKey(key string) bool {
	switch strings.ToLower(key) {
	case "next", "cursor", "page", "pagination", "paging", "hasmore", "has_more", "hasnext", "has_next", "offset", "continuationtoken", "continuation_token", "nexttoken", "next_token", "nextpage", "next_page", "nextcursor", "next_cursor":
		return true
	default:
		return false
	}
}

func validateListCounts(body []byte, aliasCount int) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return validateListCountValue(value, aliasCount)
}

func validateListCountValue(value any, aliasCount int) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "total", "totalcount", "total_count":
				count, ok := child.(json.Number)
				if !ok {
					return fmt.Errorf("%s must be a number", key)
				}
				parsed, err := count.Int64()
				if err != nil || parsed < 0 {
					return fmt.Errorf("%s must be a non-negative integer", key)
				}
				if parsed != int64(aliasCount) {
					return fmt.Errorf("%s does not match hmeEmails length", key)
				}
			}
			if err := validateListCountValue(child, aliasCount); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateListCountValue(child, aliasCount); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAliasFields(body []byte) error {
	var fields map[string]json.RawMessage
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(body, &fields) != nil {
		return errors.New("alias must be an object")
	}
	for _, key := range []string{"hme", "forwardToEmail"} {
		raw, present := fields[key]
		trimmedField := bytes.TrimSpace(raw)
		if !present || len(trimmedField) == 0 || trimmedField[0] != '"' {
			return fmt.Errorf("%s must be a string", key)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a string", key)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", key)
		}
	}
	rawActive, present := fields["isActive"]
	trimmedActive := bytes.TrimSpace(rawActive)
	if !present || (!bytes.Equal(trimmedActive, []byte("true")) && !bytes.Equal(trimmedActive, []byte("false"))) {
		return errors.New("isActive must be a boolean")
	}
	return nil
}

func requiresTwoFactor(session Session) bool {
	return session.HSAVersion >= 1 && (session.HSAChallengeRequired || !session.HSATrustedBrowser)
}

func validSecurityCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (c *Client) newUUID() (string, error) {
	bytes := make([]byte, 16)
	if err := c.readRandom(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func (c *Client) readRandom(destination []byte) error {
	c.randomMu.Lock()
	defer c.randomMu.Unlock()
	_, err := io.ReadFull(c.random, destination)
	return err
}

func validateEndpoints(region Region, endpoints Endpoints) error {
	auth, err := validateBaseURL(endpoints.Auth)
	if err != nil || auth.Hostname() != "idmsa.apple.com" || auth.Path != "/appleauth/auth" {
		return fmt.Errorf("%w: invalid auth endpoint for %s", ErrInvalidConfig, region)
	}
	home, err := validateBaseURL(endpoints.Home)
	wantHome := "www.icloud.com"
	wantSetup := "setup.icloud.com"
	if region == RegionChina {
		wantHome += ".cn"
		wantSetup += ".cn"
	}
	if err != nil || home.Hostname() != wantHome || (home.Path != "" && home.Path != "/") {
		return fmt.Errorf("%w: invalid home endpoint for %s", ErrInvalidConfig, region)
	}
	setup, err := validateBaseURL(endpoints.Setup)
	if err != nil || setup.Hostname() != wantSetup || setup.Path != "/setup/ws/1" {
		return fmt.Errorf("%w: invalid setup endpoint for %s", ErrInvalidConfig, region)
	}
	return nil
}

func validateBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
		return nil, errors.New("invalid HTTPS URL")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errors.New("non-HTTPS port")
	}
	return u, nil
}

func validAppleURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return isAppleDomain(u.Hostname())
}

func validICloudServiceURL(u *url.URL) bool {
	if !validAppleURL(u) || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	return host == "icloud.com" || strings.HasSuffix(host, ".icloud.com") || host == "icloud.com.cn" || strings.HasSuffix(host, ".icloud.com.cn")
}

func isAppleDomain(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, domain := range []string{"apple.com", "icloud.com", "apple.com.cn", "icloud.com.cn"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func validBuildIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func validHMEAnonymousID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_') {
			return false
		}
	}
	return true
}
