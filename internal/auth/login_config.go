package auth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// LoginConfig is the deployment's login configuration, from -login-config-file: the
// extra parameters of the authorization request, and which claims of the verified ID
// token /auth/session shows. Names in it, such as connector_id, are the issuer's, not
// krm-foyer's: it knows no product and interprets no value.
type LoginConfig struct {
	// AuthorizationParameters are the extra query parameters sent to the issuer's
	// authorization endpoint, by name. A parameter not named here is never sent.
	AuthorizationParameters map[string]Parameter `json:"authorizationParameters,omitempty"`
	// SessionClaims say where in the ID token's claims /auth/session finds what it
	// shows besides the issuer, subject and email.
	SessionClaims SessionClaims `json:"sessionClaims,omitempty"`
}

// Parameter is one extra parameter of the authorization request.
type Parameter struct {
	// Default is sent when the login request gives no value. Empty sends nothing.
	Default string `json:"default,omitempty"`
	// AllowFromRequest lets the login request give a value, as oidc.<name>.
	AllowFromRequest bool `json:"allowFromRequest,omitempty"`
	// AllowedValues, when set, are the only values sent, the default among them.
	AllowedValues []string `json:"allowedValues,omitempty"`
}

// SessionClaims are JSON Pointers (RFC 6901) into the verified ID token's claims.
type SessionClaims struct {
	// DisplayName is a string. Empty means /name.
	DisplayName string `json:"displayName,omitempty"`
	// Groups is a list of strings. Empty means /groups.
	Groups string `json:"groups,omitempty"`
	// Connector is a string, such as the connector a federating issuer signed the
	// user in with. Empty shows none.
	Connector string `json:"connector,omitempty"`
}

// ParseLoginConfig reads a login configuration, YAML or JSON. A key it does not know
// is an error, so a misspelt one fails startup rather than doing nothing.
func ParseLoginConfig(data []byte) (LoginConfig, error) {
	var c LoginConfig
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return LoginConfig{}, err
	}
	return c, nil
}

// Validate checks c as New would, so a bad configuration stops krm-foyer at start.
func (c LoginConfig) Validate() error {
	if _, err := newParameters(c.AuthorizationParameters); err != nil {
		return err
	}
	_, err := newClaimPaths(c.SessionClaims)
	return err
}

// Bounds on what a login request may add to the authorization request. They are
// generic: no value is judged by what it means to the issuer.
const (
	// maxParameters is the most extra parameters a deployment configures.
	maxParameters = 16
	// maxParameterValue is the longest value sent, a default included.
	maxParameterValue = 512
	// maxRequestParameters is the most bytes of names and values a login request may
	// add, which its login cookie then holds.
	maxRequestParameters = 1024
	// maxAllowedValues is the most values a parameter may allow.
	maxAllowedValues = 64
)

// RequestParameterPrefix marks the query parameters of /auth/login that are meant for
// the issuer: oidc.<name>. The rest of the name is the parameter's.
const RequestParameterPrefix = "oidc."

// parameterName is what a configured name may be: a plain query parameter name.
var parameterName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// reservedParameters are the authorization request's own parameters, which krm-foyer
// sets, and those that would replace the request or carry a credential. None may be
// configured, so none can come from a login request either.
var reservedParameters = []string{
	"client_id", "redirect_uri", "response_type", "response_mode", "scope", "state", "nonce",
	"code_challenge", "code_challenge_method", "request", "request_uri",
	"client_secret", "client_assertion", "client_assertion_type",
}

// parameters are the checked AuthorizationParameters.
type parameters map[string]Parameter

func newParameters(configured map[string]Parameter) (parameters, error) {
	if len(configured) > maxParameters {
		return nil, fmt.Errorf("%d authorization parameters, at most %d", len(configured), maxParameters)
	}
	ps := parameters{}
	for name, p := range configured {
		switch {
		case !parameterName.MatchString(name):
			return nil, fmt.Errorf("authorization parameter %q: a name is 1 to 64 letters, digits, _, . or -", name)
		case slices.Contains(reservedParameters, strings.ToLower(name)):
			return nil, fmt.Errorf("authorization parameter %q is krm-foyer's own, or would replace the request", name)
		case p.Default == "" && !p.AllowFromRequest:
			return nil, fmt.Errorf("authorization parameter %q has no default and allows no value from the request: it would never be sent", name)
		case len(p.Default) > maxParameterValue:
			return nil, fmt.Errorf("authorization parameter %q: the default is longer than %d bytes", name, maxParameterValue)
		case len(p.AllowedValues) > maxAllowedValues:
			return nil, fmt.Errorf("authorization parameter %q allows %d values, at most %d", name, len(p.AllowedValues), maxAllowedValues)
		}
		seen := map[string]bool{}
		for _, v := range p.AllowedValues {
			if v == "" || len(v) > maxParameterValue || seen[v] {
				return nil, fmt.Errorf("authorization parameter %q: allowed values must be distinct, non-empty and at most %d bytes", name, maxParameterValue)
			}
			seen[v] = true
		}
		if p.Default != "" && p.AllowedValues != nil && !seen[p.Default] {
			return nil, fmt.Errorf("authorization parameter %q: the default is not one of its allowed values", name)
		}
		ps[name] = Parameter{Default: p.Default, AllowFromRequest: p.AllowFromRequest, AllowedValues: slices.Clone(p.AllowedValues)}
	}
	return ps, nil
}

// errParameterRefused is every refusal of a login request's oidc.* parameters. It
// never says which or why in more than a word: the values may be anything.
var errParameterRefused = errors.New("login parameters refused")

// fromRequest returns the values q gives for the issuer, by parameter name: only
// configured parameters that allow one, each given exactly once, non-empty, within
// its allowed values and within the bounds. Anything else is errParameterRefused.
// Values are passed on exactly as given: never trimmed, folded or otherwise changed.
func (ps parameters) fromRequest(q url.Values) (map[string]string, error) {
	given := map[string]string{}
	size := 0
	for key, values := range q {
		name, ok := strings.CutPrefix(key, RequestParameterPrefix)
		if !ok {
			continue
		}
		p, known := ps[name]
		if !known || !p.AllowFromRequest || len(values) != 1 || values[0] == "" || len(values[0]) > maxParameterValue ||
			(p.AllowedValues != nil && !slices.Contains(p.AllowedValues, values[0])) {
			return nil, errParameterRefused
		}
		size += len(name) + len(values[0])
		given[name] = values[0]
	}
	if size > maxRequestParameters {
		return nil, errParameterRefused
	}
	return given, nil
}

// send is what the authorization request carries besides krm-foyer's own: each
// configured default, replaced by the value the login request gave.
func (ps parameters) send(given map[string]string) map[string]string {
	out := map[string]string{}
	for name, p := range ps {
		if p.Default != "" {
			out[name] = p.Default
		}
	}
	for name, v := range given {
		out[name] = v
	}
	return out
}

// retry is the query that starts a login like the one that failed: its return path,
// and the values it gave for parameters that have a list of allowed values, such as a
// connector. A free value, such as a login hint, is opaque and may be anyone's, so it
// is not repeated on an error page: the application gives a fresh link.
func (ps parameters) retry(returnTo string, given map[string]string) string {
	q := url.Values{"return_to": {returnTo}}
	for name, v := range given {
		if p, ok := ps[name]; ok && p.AllowedValues != nil {
			q.Set(RequestParameterPrefix+name, v)
		}
	}
	return "/auth/login?" + q.Encode()
}
