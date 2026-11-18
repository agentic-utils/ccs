package mcp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// accessVerifier checks the Cf-Access-Jwt-Assertion token Cloudflare Access
// adds to every request it lets through: signed (RS256) by one of the team's
// keys, for this application (aud), issued by the team, current, and for an
// allowed email. ccs serve checks it itself, so a tunnel left without an
// Access policy, or anything on this Mac reaching the loopback port, gets
// nothing.
type accessVerifier struct {
	team   string // the team domain, e.g. me.cloudflareaccess.com
	aud    string // the Access application's AUD tag
	emails []string
	fetch  func(url string) ([]byte, error) // the team's certs; stubbed in tests
	now    func() time.Time

	fetchMu sync.Mutex // one certs fetch at a time, never under mu
	mu      sync.Mutex // guards keys, loaded and tried
	keys    map[string]*rsa.PublicKey
	loaded  time.Time // when keys was last fetched successfully
	tried   time.Time // the last fetch attempt
}

// accessSkew is the clock difference allowed on exp and nbf.
const accessSkew = time.Minute

// accessRefetch is the least time between fetches of the team's certs, so a
// stream of tokens with made-up key ids can't make ccs hammer Cloudflare.
const accessRefetch = 30 * time.Second

// accessKeysFresh is how long fetched keys are used before fetching again,
// so a key Cloudflare retires stops working here too; accessKeysStale is
// how long they're still used when that fetch fails, after which every
// token is refused.
const (
	accessKeysFresh = time.Hour
	accessKeysStale = 6 * time.Hour
)

func newAccessVerifier(team, aud string, emails []string) *accessVerifier {
	if !strings.Contains(team, ".") {
		team += ".cloudflareaccess.com"
	}
	lower := make([]string, 0, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			lower = append(lower, e)
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return &accessVerifier{
		team: team, aud: aud, emails: lower, now: time.Now,
		fetch: func(url string) ([]byte, error) {
			resp, err := client.Get(url)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("%s: %s", url, resp.Status)
			}
			return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		},
	}
}

func (v *accessVerifier) certsURL() string { return "https://" + v.team + "/cdn-cgi/access/certs" }

// key returns the team's public key with this id. Keys younger than
// accessKeysFresh are used as they are; otherwise (or for an id not known)
// the certs are fetched again, at most every accessRefetch, outside mu so a
// slow fetch never holds up tokens whose key is fresh. A failed fetch falls
// back to keys younger than accessKeysStale, else refuses.
func (v *accessVerifier) key(kid string) (*rsa.PublicKey, error) {
	if k, fresh := v.cached(kid); k != nil && fresh {
		return k, nil
	}
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	k, fresh := v.cached(kid) // another request may have just fetched
	if k != nil && fresh {
		return k, nil
	}
	v.mu.Lock()
	recent := !v.tried.IsZero() && v.now().Sub(v.tried) < accessRefetch
	if !recent {
		v.tried = v.now()
	}
	v.mu.Unlock()
	if !recent {
		keys, err := v.fetchKeys()
		if err == nil {
			v.mu.Lock()
			v.keys, v.loaded = keys, v.now()
			v.mu.Unlock()
			if k := keys[kid]; k != nil {
				return k, nil
			}
			return nil, errors.New("unknown signing key")
		}
		if k == nil || !v.usable() {
			return nil, err
		}
		return k, nil
	}
	if k != nil && v.usable() {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

// cached is the key with this id, and whether the keys are fresh.
func (v *accessVerifier) cached(kid string) (*rsa.PublicKey, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.keys[kid], !v.loaded.IsZero() && v.now().Sub(v.loaded) < accessKeysFresh
}

// usable: the keys are old but not past accessKeysStale.
func (v *accessVerifier) usable() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.loaded.IsZero() && v.now().Sub(v.loaded) < accessKeysStale
}

func (v *accessVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	body, err := v.fetch(v.certsURL())
	if err != nil {
		return nil, fmt.Errorf("couldn't fetch Access certs: %v", err)
	}
	var set struct {
		Keys []struct {
			Kid, Kty, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("bad Access certs: %v", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("Access certs hold no RSA keys")
	}
	return keys, nil
}

// verify returns the token's email when it passes every check.
func (v *accessVerifier) verify(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var head struct{ Alg, Kid string }
	if err := decodeSegment(parts[0], &head); err != nil {
		return "", err
	}
	if head.Alg != "RS256" {
		return "", fmt.Errorf("token algorithm %q not allowed", head.Alg)
	}
	key, err := v.key(head.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("malformed signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	var claims struct {
		Aud   audience `json:"aud"`
		Iss   string   `json:"iss"`
		Exp   int64    `json:"exp"`
		Nbf   int64    `json:"nbf"`
		Email string   `json:"email"`
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", err
	}
	now := v.now()
	switch {
	case !slices.Contains(claims.Aud, v.aud):
		return "", errors.New("token is for another application")
	case claims.Iss != "https://"+v.team:
		return "", errors.New("token is from another team")
	case claims.Exp == 0 || now.After(time.Unix(claims.Exp, 0).Add(accessSkew)):
		return "", errors.New("token expired")
	case claims.Nbf != 0 && now.Add(accessSkew).Before(time.Unix(claims.Nbf, 0)):
		return "", errors.New("token not valid yet")
	}
	email := strings.ToLower(claims.Email)
	if email == "" || !slices.Contains(v.emails, email) {
		return "", fmt.Errorf("%q is not allowed", claims.Email)
	}
	return email, nil
}

func decodeSegment(seg string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("malformed token")
	}
	if err := json.Unmarshal(b, into); err != nil {
		return errors.New("malformed token")
	}
	return nil
}

// audience is a JWT aud: one string or a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}
