package apple

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Region selects the iCloud service cluster. Apple ID authentication itself
// always uses the global IDMSA endpoint.
type Region string

const (
	RegionGlobal Region = "global"
	RegionChina  Region = "cn"
)

const (
	defaultWidgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
	defaultBuild     = "2534Project66"
	defaultMastering = "2534B22"
)

// Endpoints contains the three Apple service roots used by this package.
// NewClient validates every endpoint before accepting it.
type Endpoints struct {
	Auth  string
	Home  string
	Setup string
}

// DefaultEndpoints returns the production Apple endpoints for region.
func DefaultEndpoints(region Region) (Endpoints, error) {
	switch region {
	case "", RegionGlobal:
		return Endpoints{
			Auth:  "https://idmsa.apple.com/appleauth/auth",
			Home:  "https://www.icloud.com",
			Setup: "https://setup.icloud.com/setup/ws/1",
		}, nil
	case RegionChina:
		return Endpoints{
			Auth:  "https://idmsa.apple.com/appleauth/auth",
			Home:  "https://www.icloud.com.cn",
			Setup: "https://setup.icloud.com.cn/setup/ws/1",
		}, nil
	default:
		return Endpoints{}, fmt.Errorf("%w: unknown region %q", ErrInvalidConfig, region)
	}
}

// Session is the complete resumable Apple web session. It intentionally has
// no password field and can be encrypted and persisted as JSON by callers.
type Session struct {
	Region                      Region             `json:"region"`
	AppleID                     string             `json:"apple_id,omitempty"`
	CountryCode                 string             `json:"country_code,omitempty"`
	SessionID                   string             `json:"session_id,omitempty"`
	SessionToken                string             `json:"session_token,omitempty"`
	TrustToken                  string             `json:"trust_token,omitempty"`
	SCNT                        string             `json:"scnt,omitempty"`
	AuthAttributes              string             `json:"auth_attributes,omitempty"`
	FrameID                     string             `json:"frame_id,omitempty"`
	ClientID                    string             `json:"client_id,omitempty"`
	DSID                        string             `json:"dsid,omitempty"`
	PrimaryEmail                string             `json:"primary_email,omitempty"`
	PremiumMailSettingsURL      string             `json:"premium_mail_settings_url,omitempty"`
	HSAVersion                  int                `json:"hsa_version,omitempty"`
	HSAChallengeRequired        bool               `json:"hsa_challenge_required,omitempty"`
	HSATrustedBrowser           bool               `json:"hsa_trusted_browser,omitempty"`
	HideMyEmailActive           bool               `json:"hide_my_email_active,omitempty"`
	HideMyEmailFeatureAvailable bool               `json:"hide_my_email_feature_available,omitempty"`
	Cookies                     []PersistentCookie `json:"cookies,omitempty"`
	ValidatedAt                 time.Time          `json:"validated_at,omitempty"`
}

// Alias is one Hide My Email address returned by Apple's premium mail service.
type Alias struct {
	Origin          string `json:"origin"`
	OriginAppName   string `json:"originAppName,omitempty"`
	AppIconURL      string `json:"appIconUrl,omitempty"`
	AppBundleID     string `json:"appBundleId,omitempty"`
	AnonymousID     string `json:"anonymousId"`
	Domain          string `json:"domain"`
	ForwardToEmail  string `json:"forwardToEmail"`
	HME             string `json:"hme"`
	IsActive        bool   `json:"isActive"`
	Label           string `json:"label"`
	Note            string `json:"note"`
	CreateTimestamp int64  `json:"createTimestamp"`
	RecipientMailID string `json:"recipientMailId"`
}

// CreatedAt converts Apple's millisecond timestamp to UTC.
func (a Alias) CreatedAt() time.Time {
	return time.UnixMilli(a.CreateTimestamp).UTC()
}

// ListResult is the authoritative full Hide My Email list. It includes both
// active and deactivated aliases; IsActive differentiates them.
type ListResult struct {
	Aliases           []Alias  `json:"hmeEmails"`
	SelectedForwardTo string   `json:"selectedForwardTo"`
	ForwardToEmails   []string `json:"forwardToEmails"`
}

type accountResponse struct {
	Success              *bool                 `json:"success,omitempty"`
	DSInfo               dsInfo                `json:"dsInfo"`
	Webservices          map[string]webservice `json:"webservices"`
	HSAChallengeRequired bool                  `json:"hsaChallengeRequired"`
	HSATrustedBrowser    bool                  `json:"hsaTrustedBrowser"`
}

type dsInfo struct {
	DSID                        stringish `json:"dsid"`
	AppleID                     string    `json:"appleId"`
	PrimaryEmail                string    `json:"primaryEmail"`
	CountryCode                 string    `json:"countryCode"`
	HSAVersion                  int       `json:"hsaVersion"`
	HideMyEmailActive           bool      `json:"isHideMyEmailSubscriptionActive"`
	HideMyEmailFeatureAvailable bool      `json:"isHideMyEmailFeatureAvailable"`
}

type webservice struct {
	URL    string `json:"url"`
	Status string `json:"status"`
}

func (s *Session) applyAccount(response accountResponse) {
	s.DSID = string(response.DSInfo.DSID)
	s.PrimaryEmail = response.DSInfo.PrimaryEmail
	if s.PrimaryEmail == "" {
		s.PrimaryEmail = response.DSInfo.AppleID
	}
	if response.DSInfo.CountryCode != "" {
		s.CountryCode = response.DSInfo.CountryCode
	}
	s.HSAVersion = response.DSInfo.HSAVersion
	s.HSAChallengeRequired = response.HSAChallengeRequired
	s.HSATrustedBrowser = response.HSATrustedBrowser
	s.HideMyEmailActive = response.DSInfo.HideMyEmailActive
	s.HideMyEmailFeatureAvailable = response.DSInfo.HideMyEmailFeatureAvailable
	if premium, ok := response.Webservices["premiummailsettings"]; ok {
		s.PremiumMailSettingsURL = strings.TrimRight(premium.URL, "/")
	}
}

// stringish accepts Apple's DSID in either string or JSON-number form.
type stringish string

func (s *stringish) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = stringish(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	if _, err := strconv.ParseUint(string(number), 10, 64); err != nil {
		return err
	}
	*s = stringish(number)
	return nil
}
