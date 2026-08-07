package apple

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// PersistentCookie is the JSON-safe representation retained alongside a
// Session. Domain, path and host-only semantics are preserved across restarts.
type PersistentCookie struct {
	Name     string        `json:"name"`
	Value    string        `json:"value"`
	Domain   string        `json:"domain"`
	Path     string        `json:"path"`
	HostOnly bool          `json:"host_only,omitempty"`
	Expires  time.Time     `json:"expires,omitempty"`
	Secure   bool          `json:"secure,omitempty"`
	HTTPOnly bool          `json:"http_only,omitempty"`
	SameSite http.SameSite `json:"same_site,omitempty"`
}

// PersistentJar delegates RFC cookie matching to net/http/cookiejar while
// retaining enough metadata to export and restore the jar.
type PersistentJar struct {
	mu      sync.Mutex
	jar     *cookiejar.Jar
	records map[string]PersistentCookie
}

// NewPersistentJar constructs a standard-library cookie jar and imports the
// supplied persisted cookies.
func NewPersistentJar(cookies []PersistentCookie) (*PersistentJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create cookie jar: %v", ErrInvalidSession, err)
	}
	result := &PersistentJar{jar: jar, records: make(map[string]PersistentCookie)}
	if err := result.Import(cookies); err != nil {
		return nil, err
	}
	return result, nil
}

// Cookies implements http.CookieJar.
func (j *PersistentJar) Cookies(u *url.URL) []*http.Cookie {
	if j == nil || j.jar == nil {
		return nil
	}
	return j.jar.Cookies(u)
}

// SetCookies implements http.CookieJar and updates the exportable metadata.
func (j *PersistentJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if j == nil || j.jar == nil || u == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar.SetCookies(u, cookies)
	now := time.Now()
	for _, cookie := range cookies {
		if cookie == nil || cookie.Name == "" {
			continue
		}
		record := persistentCookieFromHTTP(u, cookie, now)
		key := cookieRecordKey(record)
		if cookie.MaxAge < 0 || (!record.Expires.IsZero() && !record.Expires.After(now)) {
			delete(j.records, key)
			continue
		}
		j.records[key] = record
	}
}

// Export returns a stable, deep copy suitable for JSON persistence.
func (j *PersistentJar) Export() []PersistentCookie {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	result := make([]PersistentCookie, 0, len(j.records))
	for key, cookie := range j.records {
		if !cookie.Expires.IsZero() && !cookie.Expires.After(now) {
			delete(j.records, key)
			continue
		}
		result = append(result, cookie)
	}
	sort.Slice(result, func(i, k int) bool {
		return cookieRecordKey(result[i]) < cookieRecordKey(result[k])
	})
	return result
}

// Import restores cookies without extending their original expiry.
func (j *PersistentJar) Import(cookies []PersistentCookie) error {
	if j == nil || j.jar == nil {
		return fmt.Errorf("%w: empty cookie jar", ErrInvalidSession)
	}
	now := time.Now()
	for _, saved := range cookies {
		if err := validatePersistentCookie(saved); err != nil {
			return err
		}
		if !saved.Expires.IsZero() && !saved.Expires.After(now) {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(saved.Domain), ".")
		u := &url.URL{Scheme: "https", Host: host, Path: saved.Path}
		cookie := &http.Cookie{
			Name:     saved.Name,
			Value:    saved.Value,
			Path:     saved.Path,
			Expires:  saved.Expires,
			Secure:   saved.Secure,
			HttpOnly: saved.HTTPOnly,
			SameSite: saved.SameSite,
		}
		if !saved.HostOnly {
			cookie.Domain = "." + host
		}
		j.SetCookies(u, []*http.Cookie{cookie})
	}
	return nil
}

func persistentCookieFromHTTP(u *url.URL, cookie *http.Cookie, now time.Time) PersistentCookie {
	domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
	hostOnly := domain == ""
	if hostOnly {
		domain = strings.ToLower(u.Hostname())
	}
	path := cookie.Path
	if path == "" || path[0] != '/' {
		path = defaultCookiePath(u.Path)
	}
	expires := cookie.Expires
	if cookie.MaxAge > 0 {
		expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
	}
	return PersistentCookie{
		Name:     cookie.Name,
		Value:    cookie.Value,
		Domain:   domain,
		Path:     path,
		HostOnly: hostOnly,
		Expires:  expires,
		Secure:   cookie.Secure,
		HTTPOnly: cookie.HttpOnly,
		SameSite: cookie.SameSite,
	}
}

func validatePersistentCookie(cookie PersistentCookie) error {
	domain := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
	if cookie.Name == "" || strings.ContainsAny(cookie.Name, "\x00\r\n;,") {
		return fmt.Errorf("%w: invalid cookie name", ErrInvalidSession)
	}
	if strings.ContainsAny(cookie.Value, "\x00\r\n;") {
		return fmt.Errorf("%w: invalid cookie value", ErrInvalidSession)
	}
	if domain == "" || strings.ContainsAny(domain, "\x00\r\n/:@") || strings.Contains(domain, "..") {
		return fmt.Errorf("%w: invalid cookie domain", ErrInvalidSession)
	}
	if cookie.Path == "" || cookie.Path[0] != '/' || strings.ContainsAny(cookie.Path, "\x00\r\n") {
		return fmt.Errorf("%w: invalid cookie path", ErrInvalidSession)
	}
	return nil
}

func cookieRecordKey(cookie PersistentCookie) string {
	return fmt.Sprintf("%t\x00%s\x00%s\x00%s", cookie.HostOnly, strings.ToLower(cookie.Domain), cookie.Path, cookie.Name)
}

func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' || requestPath == "/" {
		return "/"
	}
	lastSlash := strings.LastIndex(requestPath, "/")
	if lastSlash <= 0 {
		return "/"
	}
	return requestPath[:lastSlash]
}
