package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// pointer is a parsed JSON Pointer (RFC 6901): the reference tokens, unescaped.
type pointer []string

// parsePointer parses s, which must name something inside the document: it starts
// with /. A ~ must be ~0 or ~1.
func parsePointer(s string) (pointer, error) {
	if !strings.HasPrefix(s, "/") {
		return nil, fmt.Errorf("JSON Pointer %q does not start with /", s)
	}
	var p pointer
	for _, token := range strings.Split(s[1:], "/") {
		for i := 0; i < len(token); i++ {
			if token[i] != '~' {
				continue
			}
			if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
				return nil, fmt.Errorf("JSON Pointer %q has a ~ that is not ~0 or ~1", s)
			}
			i++
		}
		p = append(p, strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~"))
	}
	return p, nil
}

// lookup returns what p names in doc, a document as encoding/json decodes one, and
// whether there is anything there. An object member is named by its key, an array
// element by its index in decimal; anything else names nothing.
func (p pointer) lookup(doc any) (any, bool) {
	for _, token := range p {
		switch v := doc.(type) {
		case map[string]any:
			next, ok := v[token]
			if !ok {
				return nil, false
			}
			doc = next
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != token {
				return nil, false
			}
			doc = v[i]
		default:
			return nil, false
		}
	}
	return doc, true
}

// claimPaths are the checked SessionClaims.
type claimPaths struct {
	displayName, groups, connector pointer
}

func newClaimPaths(c SessionClaims) (claimPaths, error) {
	var cp claimPaths
	var err error
	if c.DisplayName == "" {
		c.DisplayName = "/name"
	}
	if c.Groups == "" {
		c.Groups = "/groups"
	}
	if cp.displayName, err = parsePointer(c.DisplayName); err != nil {
		return claimPaths{}, fmt.Errorf("sessionClaims.displayName: %w", err)
	}
	if cp.groups, err = parsePointer(c.Groups); err != nil {
		return claimPaths{}, fmt.Errorf("sessionClaims.groups: %w", err)
	}
	if c.Connector != "" {
		if cp.connector, err = parsePointer(c.Connector); err != nil {
			return claimPaths{}, fmt.Errorf("sessionClaims.connector: %w", err)
		}
	}
	return cp, nil
}

// identity is what /auth/session shows of the user besides the issuer, subject and
// email, all from claims the issuer signed. Kubernetes decides access; these are for
// the application to show.
type identity struct {
	DisplayName string
	Groups      []string
	Connector   string
}

// errClaimType means a claim the configuration names is there, with the wrong type.
var errClaimType = errors.New("a session claim has the wrong type")

// extract reads the identity from claims. A claim that is absent, or null, is empty;
// one of another type is errClaimType.
func (cp claimPaths) extract(claims map[string]any) (identity, error) {
	id := identity{Groups: []string{}}
	str := func(p pointer) (string, error) {
		v, ok := p.lookup(claims)
		if !ok || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", errClaimType
		}
		return s, nil
	}
	var err error
	if id.DisplayName, err = str(cp.displayName); err != nil {
		return identity{}, err
	}
	if cp.connector != nil {
		if id.Connector, err = str(cp.connector); err != nil {
			return identity{}, err
		}
	}
	if v, ok := cp.groups.lookup(claims); ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return identity{}, errClaimType
		}
		for _, g := range list {
			s, ok := g.(string)
			if !ok {
				return identity{}, errClaimType
			}
			id.Groups = append(id.Groups, s)
		}
	}
	return id, nil
}

// tokenClaims decodes the claims of a JWT krm-foyer verified at login. It does not
// verify it again: the token comes from the session, which only krm-foyer could seal.
func tokenClaims(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWS in compact form")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}
