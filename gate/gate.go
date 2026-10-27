// Package gate is an app's side of the fleet's identity service: it verifies a
// login ticket, and a revocation command, with nothing but a public key.
//
// Every app of the fleet imports this one package (it used to be copied into
// each of them), so two things about it matter more than usual:
//
//   - It is verify-only. Nothing here can mint a ticket, so a compromise of one
//     app cannot produce credentials for the next one.
//   - It ships with gate_test.go and its GOLDEN VECTOR: a fixed public key and a
//     fixed accepted ticket, byte for byte identical to the one in gate's own
//     repository. That test is the only thing that catches this verifier drifting
//     out of compatibility with the service it verifies.
//
// What gate hands over is a coupon, not a credential: 60 seconds, one app, one
// use. The app trades it immediately for a row in its own session table, which is
// what it already knows how to revoke.
package gate

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The wire format, which gate owns and this file only reads.
const (
	// Prefix names the format and the algorithm at once, and it is part of the
	// signed bytes: a future gt2 signature can never be replayed as a gt1 one.
	Prefix = "gt1"
	// Version is the payload version this verifier accepts. Anything else is
	// refused rather than guessed — while UNKNOWN FIELDS are ignored, which is
	// what lets gate add a claim without breaking a copy that is a version behind.
	Version = 1

	// Skew is how far in the past an issuing clock may be: one host runs every
	// container here, so this covers a restore or an NTP step.
	Skew = 120 * time.Second
	// MaxLifetime caps what this verifier accepts as a ticket's own window,
	// whatever the signer claimed. Without it, a compromised gate could mint a
	// ticket valid for a year and every app would honour it.
	MaxLifetime = 10 * time.Minute
	// MaxLen bounds the work done on an unauthenticated string: the payload has to
	// be parsed before the signature can be checked (the key id lives inside it),
	// so the input is small by contract.
	MaxLen = 4096

	// StateLifetime is how long the anti-forgery cookie survives. It only has to
	// outlive one sign-in.
	StateLifetime = 5 * time.Minute
)

// Payload actions and revocation kinds.
const (
	ActLogin  = "login"
	ActRevoke = "revoke"

	KindSub = "sub" // everything belonging to an account
	KindSid = "sid" // everything minted by one gate session
)

// Errors a verifier reports. Distinguishable on purpose: "unknown key" during a
// rotation and "bad signature" from an attacker deserve different log lines.
var (
	ErrMalformed   = errors.New("gate ticket: malformed")
	ErrVersion     = errors.New("gate ticket: unsupported version")
	ErrUnknownKey  = errors.New("gate ticket: unknown key id")
	ErrSignature   = errors.New("gate ticket: bad signature")
	ErrAudience    = errors.New("gate ticket: issued for another app")
	ErrExpired     = errors.New("gate ticket: expired")
	ErrFromFuture  = errors.New("gate ticket: issued too far in the future")
	ErrTooLongLife = errors.New("gate ticket: lifetime beyond what a verifier accepts")
	ErrIncomplete  = errors.New("gate ticket: missing a required field")
	ErrKind        = errors.New("gate ticket: not the expected payload")
	ErrState       = errors.New("gate: this sign-in did not start in this browser")
)

// Login is gate's statement that a browser is an account and may enter this app.
type Login struct {
	V   int      `json:"v"`
	Kid string   `json:"kid"`
	Act string   `json:"act"`
	Aud string   `json:"aud"`
	Sub string   `json:"sub"`
	Eml string   `json:"eml,omitempty"`
	Rol []string `json:"rol,omitempty"`
	Sid string   `json:"sid"`
	Jti string   `json:"jti"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

// Revoke is gate telling this app to drop sessions. The signature IS the
// authentication of the call, which is why the endpoint receiving it needs no
// secret of its own.
type Revoke struct {
	V     int    `json:"v"`
	Kid   string `json:"kid"`
	Act   string `json:"act"`
	Aud   string `json:"aud"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
	ID    string `json:"id"`
	Jti   string `json:"jti"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// Config is what this app knows about gate. The zero value means "gate is off",
// and an app with gate off is byte-for-byte the app it was before — which is what
// makes adopting this a change you can stop halfway through.
type Config struct {
	// AppID is the audience this app requires. A ticket for another app is refused,
	// always: that is what keeps a ticket stolen here useless next door.
	AppID string
	// URL is gate's public base URL, where a signed-out visitor is sent.
	URL string
	// Keys are the signing keys trusted, by id. A SET and not one key: appending
	// the next key everywhere, then switching gate over, then dropping the old one
	// is what makes a rotation a two-step deploy instead of a flag day.
	Keys map[string]ed25519.PublicKey
	// SessionTTL is how long a session opened by a gate ticket lives, and it does
	// NOT slide. Gate is the authority on who may enter; a session that renewed
	// itself forever would outlive that authority, so it expires and the browser
	// goes back through gate — invisibly, since gate's own session lasts weeks.
	SessionTTL time.Duration
}

// DefaultSessionTTL is the hard cap on a gate-issued session. Twelve hours: long
// enough for a working day, short enough that it is the backstop under the
// revocation push rather than a hole.
const DefaultSessionTTL = 12 * time.Hour

// ParseConfig reads the four environment values. It returns a disabled Config
// (and no error) when no key is configured, because "gate is not wired up here
// yet" is a normal state, not a misconfiguration.
func ParseConfig(appID, gateURL, keys string, ttlHours int) (*Config, error) {
	keys = strings.TrimSpace(keys)
	appID = strings.TrimSpace(appID)
	gateURL = strings.TrimSpace(gateURL)
	// Is gate wired up here at all? The question is answered by the key set and the
	// URL, and deliberately NOT by appID: GATE_APP_ID has a default (this app's own
	// name), so it is always present and can never mean "somebody configured this".
	// Reading it as a signal would make every deployment without gate look
	// half-configured.
	if keys == "" && gateURL == "" {
		return &Config{}, nil
	}
	// From here on, gate was attempted — and a half-written attempt is an error
	// rather than a silent "off". That distinction cost an evening: GATE_APP_ID and
	// GATE_PUBLIC_KEYS were right, GATE_URL was missing, and the old rule ("no keys
	// means off") turned one absent variable into a service that booted normally,
	// showed no sign-in button, and answered 404 on the revocation endpoint while
	// gate retried for hours. Nothing anywhere said why.
	if keys == "" {
		return nil, errors.New("GATE_URL is set but GATE_PUBLIC_KEYS is empty: set both, or clear both to turn gate off here")
	}
	// An empty URL would be caught two lines below, as "not an absolute URL". This
	// branch exists for the message alone: a forgotten variable and a mistyped one
	// are different mistakes, and this is by far the more common of the two.
	if gateURL == "" {
		return nil, errors.New("GATE_PUBLIC_KEYS is set but GATE_URL is empty: there would be nowhere to send anybody to sign in")
	}
	if appID == "" {
		return nil, errors.New("GATE_PUBLIC_KEYS is set but GATE_APP_ID is empty: this app would accept a ticket for any audience")
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(gateURL), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("GATE_URL must be an absolute URL, got %q", gateURL)
	}
	parsed, err := ParsePublicKeys(keys)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(ttlHours) * time.Hour
	if ttlHours <= 0 {
		ttl = DefaultSessionTTL
	}
	return &Config{AppID: appID, URL: u.String(), Keys: parsed, SessionTTL: ttl}, nil
}

// Enabled reports whether gate is wired up here.
func (c *Config) Enabled() bool { return c != nil && len(c.Keys) > 0 && c.AppID != "" }

// AuthorizeURL is where a signed-out visitor is sent to be authenticated.
func (c *Config) AuthorizeURL(state string) string {
	q := url.Values{"app": {c.AppID}, "state": {state}}
	return c.URL + "/authorize?" + q.Encode()
}

// VerifyLogin checks a ticket and returns what it claims.
//
// Replay is deliberately NOT checked here: single use needs storage, and storage
// belongs to the caller. A verified ticket is one the caller must still refuse the
// second time — see the Jti field.
func (c *Config) VerifyLogin(raw string, now time.Time) (*Login, error) {
	var t Login
	if err := c.open(raw, &t); err != nil {
		return nil, err
	}
	if t.Act != ActLogin {
		return nil, fmt.Errorf("%w: act %q", ErrKind, t.Act)
	}
	if t.Sub == "" || t.Sid == "" || t.Jti == "" {
		return nil, ErrIncomplete
	}
	if err := c.checkAudienceAndWindow(t.V, t.Aud, t.Iat, t.Exp, now); err != nil {
		return nil, err
	}
	return &t, nil
}

// VerifyRevoke checks a revocation command.
func (c *Config) VerifyRevoke(raw string, now time.Time) (*Revoke, error) {
	var r Revoke
	if err := c.open(raw, &r); err != nil {
		return nil, err
	}
	if r.Act != ActRevoke {
		return nil, fmt.Errorf("%w: act %q", ErrKind, r.Act)
	}
	if r.Value == "" || r.ID == "" || r.Jti == "" {
		return nil, ErrIncomplete
	}
	if r.Kind != KindSub && r.Kind != KindSid {
		return nil, fmt.Errorf("%w: kind %q", ErrIncomplete, r.Kind)
	}
	if err := c.checkAudienceAndWindow(r.V, r.Aud, r.Iat, r.Exp, now); err != nil {
		return nil, err
	}
	return &r, nil
}

var enc = base64.RawURLEncoding

// open splits raw, verifies the signature against the key its payload names, and
// unmarshals the payload into dst.
//
// The payload is decoded before the signature is checked — the key id lives inside
// it — so the ordering is worth being explicit about: decoding is not trusting.
// Nothing from dst may be used until this returns nil.
func (c *Config) open(raw string, dst any) error {
	if !c.Enabled() {
		return errors.New("gate: not configured here")
	}
	if len(raw) == 0 || len(raw) > MaxLen {
		return ErrMalformed
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != Prefix || parts[1] == "" || parts[2] == "" {
		return ErrMalformed
	}
	body, err := enc.DecodeString(parts[1])
	if err != nil {
		return ErrMalformed
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrMalformed
	}
	var head struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return ErrMalformed
	}
	pub, ok := c.Keys[head.Kid]
	if !ok || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: %q", ErrUnknownKey, head.Kid)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return ErrSignature
	}
	return json.Unmarshal(body, dst)
}

func (c *Config) checkAudienceAndWindow(v int, aud string, iat, exp int64, now time.Time) error {
	if v != Version {
		return fmt.Errorf("%w: v%d", ErrVersion, v)
	}
	if c.AppID == "" || aud != c.AppID {
		return fmt.Errorf("%w: %q, this is %q", ErrAudience, aud, c.AppID)
	}
	if exp <= iat || exp-iat > int64(MaxLifetime/time.Second) {
		return ErrTooLongLife
	}
	if now.Unix() >= exp {
		return ErrExpired
	}
	if now.Unix() < iat-int64(Skew/time.Second) {
		return ErrFromFuture
	}
	return nil
}

// ParsePublicKeys reads "kid:base64url kid2:base64url" (commas, spaces or newlines
// separate entries).
func ParsePublicKeys(s string) (map[string]ed25519.PublicKey, error) {
	out := map[string]ed25519.PublicKey{}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		kid, raw, ok := strings.Cut(field, ":")
		if !ok || kid == "" || raw == "" {
			return nil, fmt.Errorf("GATE_PUBLIC_KEYS: %q is not kid:key", field)
		}
		b, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("GATE_PUBLIC_KEYS: key %q: %w", kid, err)
		}
		if _, dup := out[kid]; dup {
			return nil, fmt.Errorf("GATE_PUBLIC_KEYS: %q appears twice", kid)
		}
		out[kid] = ed25519.PublicKey(b)
	}
	return out, nil
}

// decodeKey accepts base64url (padded or not) or hex, and insists on the exact
// length. Every decoder is tried and only a result of the right size is accepted,
// because the encodings overlap: a 64-character hex key is also valid base64url,
// so stopping at the first successful decode yields 48 bytes and refuses a
// perfectly good key.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, dec := range []func(string) ([]byte, error){
		enc.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == ed25519.PublicKeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not %d bytes of base64url or hex", ed25519.PublicKeySize)
}

// NewState mints the anti-forgery handle: 128 bits, base64url. It is stored in a
// short-lived cookie and echoed back by gate, so a ticket only counts in the
// browser that started the sign-in.
func NewState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return enc.EncodeToString(b), nil
}

// StateCookieValue packs the state and where the visitor was headed, so the
// return trip needs no server-side row.
func StateCookieValue(state, next string) string { return state + "|" + next }

// MatchState compares the state gate echoed with the one in the cookie, and
// returns where the visitor was headed. The comparison is constant-time out of
// habit rather than necessity: the value is single-use and short-lived, but this
// is the check that binds a ticket to a browser.
func MatchState(cookieValue, got string) (next string, ok bool) {
	state, next, found := strings.Cut(cookieValue, "|")
	if !found || state == "" || got == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(got)) != 1 {
		return "", false
	}
	return next, true
}

// SubjectFor is how a gate account is named in this app's own session table. The
// prefix is what tells a local session from a gate one at a glance, in a query and
// in a log line.
func SubjectFor(sub string) string { return "gate:" + sub }

// IsGateSubject reports whether a session was opened by a gate ticket.
func IsGateSubject(subject string) bool { return strings.HasPrefix(subject, "gate:") }

// CredFP is what a gate-issued session stores in the credential-fingerprint
// column: "gate-v1|<account>|<gate session>".
//
// Readable and NOT hashed, deliberately. A revocation naming one gate session
// carries only the session handle, never the account, so a hash of the pair could
// not be recomputed by the app receiving it — the whole per-session revocation
// would be undeliverable. There is nothing to hide here either: the account is
// already in the subject column, and the handle is itself a one-way name gate
// derived for exactly this purpose.
//
// It also keeps the column honest for gate sessions. Left holding the local
// password's fingerprint, it would claim a relationship to a credential that has
// nothing to do with how the session was opened.
func CredFP(sub, sid string) string { return "gate-v1|" + sub + "|" + sid }

// CredSuffixForSid is what a per-session revocation matches on: the tail of every
// CredFP minted by that gate session. A local fingerprint is 64 hex characters
// and can never contain a "|", so the match cannot reach a session that gate did
// not open.
func CredSuffixForSid(sid string) string { return "|" + sid }
