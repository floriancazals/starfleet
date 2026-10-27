package gate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// THE GOLDEN VECTOR. These three constants are copied byte for byte from gate's
// own repository (ticket/ticket_test.go) and must never be edited to make a test
// pass: they are the only thing that catches this verifier drifting out of
// compatibility with the service that signs the tickets.
//
// If TestGoldenTicketStillVerifies goes red, either gate changed its wire format
// or this package changed its verification — and in both cases the answer is to find
// out which, not to update the constant.
const (
	testKid     = "kTEST"
	testPubKey  = "A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"
	goldenLogin = "gt1.eyJ2IjoxLCJraWQiOiJrVEVTVCIsImFjdCI6ImxvZ2luIiwiYXVkIjoidG9kbyIsInN1YiI6ImdfN2YzYTljMmUxMWQ0IiwiZW1sIjoiYW5uZUBleGFtcGxlLmZyIiwicm9sIjpbImNvYWNoIl0sInNpZCI6InNfOWMxZjRiN2UiLCJqdGkiOiJaR1YwWlhKdGFXNXBjM1JwWTJwMGFRIiwiaWF0IjoxNzg3MDAwMDAwLCJleHAiOjE3ODcwMDAwNjB9.z1iXzmn1aC_WMZkHmC9EXonB6sSub0P4yR73W_Nqk7euwQwOBW5Va71-pvLug24ACG3zBGFuI7z5f_mPNG0ZBA"

	// The same pin for the other half of the format, and it was missing: when a
	// revocation for this app sat unconfirmed for an evening, "do the two
	// repositories still agree on what a revocation looks like" could not be
	// answered by running a test. Now it can.
	goldenRevoke = "gt1.eyJ2IjoxLCJraWQiOiJrVEVTVCIsImFjdCI6InJldm9rZSIsImF1ZCI6ImJyZWFrZmFzdCIsImtpbmQiOiJzdWIiLCJ2YWx1ZSI6ImdfN2YzYTljMmUxMWQ0IiwiaWQiOiJydl85ZDJjMWEiLCJqdGkiOiJaR1YwWlhKdGFXNXBjM1JwWTJwMGFRIiwiaWF0IjoxNzg3MDAwMDAwLCJleHAiOjE3ODcwMDAwNjB9.O8qhbD6LcTYMAWiFC-KahzIPKj9zCqukbECMMBu-wMFj3j4BMkc4p0324BFipFxOoibpqfVjdXsMZdkToPOHDw"
)

// The golden ticket names todo as its audience, so the verifier under test claims
// to be todo. What is being checked is the format and the signature, not which app
// imports this package.
func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig("todo", "https://gate.example.fr", testKid+":"+testPubKey, 12)
	if err != nil {
		t.Fatalf("parsing the test configuration: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("a configured gate reports itself disabled")
	}
	return cfg
}

func testNow() time.Time { return time.Unix(1787000000, 0).UTC() }

func TestGoldenTicketStillVerifies(t *testing.T) {
	tk, err := testConfig(t).VerifyLogin(goldenLogin, testNow().Add(30*time.Second))
	if err != nil {
		t.Fatalf("this copy can no longer verify gate's tickets: %v", err)
	}
	if tk.Sub != "g_7f3a9c2e11d4" || tk.Sid != "s_9c1f4b7e" {
		t.Errorf("claims came back wrong: %+v", tk)
	}
	if tk.Eml != "anne@example.fr" || len(tk.Rol) != 1 || tk.Rol[0] != "coach" {
		t.Errorf("identity claims came back wrong: %+v", tk)
	}
	if tk.Jti == "" {
		t.Error("no jti: single use would be impossible")
	}
}

// TestGoldenRevokeStillVerifies is the receiving end of gate's revocation queue,
// pinned. If this goes red, gate is pushing commands this app cannot read — and
// the symptom in production is not an error anywhere: it is a delivery that
// retries for ever and a "NON CONFIRMÉ" line on gate's admin page.
//
// The vector is addressed to "breakfast", so unlike the login one it is verified
// with this app's real audience.
func TestGoldenRevokeStillVerifies(t *testing.T) {
	cfg, err := ParseConfig("breakfast", "https://gate.example.fr", testKid+":"+testPubKey, 12)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	cmd, err := cfg.VerifyRevoke(goldenRevoke, testNow().Add(30*time.Second))
	if err != nil {
		t.Fatalf("this copy can no longer read gate's revocations: %v", err)
	}
	if cmd.Kind != KindSub {
		t.Errorf("kind is %q, want %q — a command this app does not understand is dropped", cmd.Kind, KindSub)
	}
	if cmd.Value != "g_7f3a9c2e11d4" {
		t.Errorf("value is %q: the wrong sessions would be deleted", cmd.Value)
	}
	// The id IS the acknowledgement: answering 200 without it makes gate retry for
	// ever, which is the failure this whole vector exists to catch early.
	if cmd.ID != "rv_9d2c1a" {
		t.Errorf("id is %q, want rv_9d2c1a", cmd.ID)
	}

	// And the containment holds on this half too: the same command is worthless in
	// another app.
	other, err := ParseConfig("todo", "https://gate.example.fr", testKid+":"+testPubKey, 12)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := other.VerifyRevoke(goldenRevoke, testNow()); !errors.Is(err, ErrAudience) {
		t.Fatalf("another app accepted a revocation addressed to breakfast: %v", err)
	}
	// A revocation is not a login, in either direction.
	if _, err := cfg.VerifyLogin(goldenRevoke, testNow()); !errors.Is(err, ErrKind) {
		t.Fatalf("a revocation was accepted as a login: %v", err)
	}
}

// TestVerifyRejects is the table that has to stay red on tampering. Every entry is
// a real way a ticket can be wrong.
func TestVerifyRejects(t *testing.T) {
	cfg := testConfig(t)
	now := testNow()

	// One character of the signature, swapped for another valid base64 character:
	// the string stays well-formed, so the signature check is what has to catch it.
	tampered := []byte(goldenLogin)
	if tampered[len(tampered)-10] == 'A' {
		tampered[len(tampered)-10] = 'B'
	} else {
		tampered[len(tampered)-10] = 'A'
	}

	for _, c := range []struct {
		name string
		raw  string
		at   time.Time
		want error
	}{
		{"empty", "", now, ErrMalformed},
		{"another prefix", "gt2" + goldenLogin[3:], now, ErrMalformed},
		{"two parts", strings.Join(strings.Split(goldenLogin, ".")[:2], "."), now, ErrMalformed},
		{"oversized", "gt1." + strings.Repeat("A", MaxLen) + ".AA", now, ErrMalformed},
		{"a tampered signature", string(tampered), now, ErrSignature},
		{"expired", goldenLogin, now.Add(61 * time.Second), ErrExpired},
		{"from the future", goldenLogin, now.Add(-5 * time.Minute), ErrFromFuture},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := cfg.VerifyLogin(c.raw, c.at)
			if got != nil {
				t.Fatalf("accepted a ticket it must refuse: %+v", got)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("refused for the wrong reason: got %v, want %v", err, c.want)
			}
		})
	}

	// The containment property, and the reason every ticket names an audience: this
	// exact ticket is worthless in any other app.
	other, err := ParseConfig("breakfast", "https://gate.example.fr", testKid+":"+testPubKey, 12)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := other.VerifyLogin(goldenLogin, now); !errors.Is(err, ErrAudience) {
		t.Fatalf("another app accepted a ticket issued for todo: %v", err)
	}

	// A key this app does not know about is refused, which is what makes a rotation
	// safe: the old key stops being trusted the moment it is dropped.
	stranger, err := ParseConfig("todo", "https://gate.example.fr", "kOTHER:"+testPubKey, 12)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, err := stranger.VerifyLogin(goldenLogin, now); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("a ticket signed under an unknown key id was accepted: %v", err)
	}

	// And a login ticket is not a revocation command, in either direction.
	if _, err := cfg.VerifyRevoke(goldenLogin, now); !errors.Is(err, ErrKind) {
		t.Fatalf("a login ticket was accepted as a revocation: %v", err)
	}
}

// A gate that is not configured must verify nothing at all — not fall back to
// accepting, which is the failure mode that matters when a key is missing from a
// deployment.
func TestDisabledVerifiesNothing(t *testing.T) {
	off, err := ParseConfig("", "", "", 0)
	if err != nil {
		t.Fatalf("an empty configuration is a normal state, not an error: %v", err)
	}
	if off.Enabled() {
		t.Fatal("an unconfigured gate reports itself enabled")
	}
	// And the app id alone is not a configuration: GATE_APP_ID has a default, so a
	// deployment that never mentions gate still carries one. If this were read as
	// intent, every such deployment would report a broken configuration.
	byDefault, err := ParseConfig("breakfast", "", "", 12)
	if err != nil {
		t.Fatalf("an app id with no keys and no url must simply mean off: %v", err)
	}
	if byDefault.Enabled() {
		t.Fatal("an app id alone enabled gate")
	}
	if _, err := off.VerifyLogin(goldenLogin, testNow()); err == nil {
		t.Fatal("an unconfigured gate verified a ticket")
	}
	var nilCfg *Config
	if nilCfg.Enabled() {
		t.Fatal("a nil configuration reports itself enabled")
	}
}

func TestParseConfigRefusals(t *testing.T) {
	for _, c := range []struct{ name, app, url, keys string }{
		{"keys without an app id", "", "https://gate.example.fr", testKid + ":" + testPubKey},
		// THE ONE THAT BIT. Everything right except GATE_URL, which used to parse
		// as "gate is off here": no button on the sign-in page, 404 on the
		// revocation endpoint, and a queue retrying against nothing.
		{"keys without a url", "breakfast", "", testKid + ":" + testPubKey},
		{"a url without keys", "breakfast", "https://gate.example.fr", ""},
		{"a relative gate url", "todo", "/gate", testKid + ":" + testPubKey},
		{"no scheme", "todo", "gate.example.fr", testKid + ":" + testPubKey},
		{"a key that is not kid:value", "todo", "https://gate.example.fr", testPubKey},
		{"a key of the wrong length", "todo", "https://gate.example.fr", "k1:AAAA"},
		{"the same id twice", "todo", "https://gate.example.fr", "k1:" + testPubKey + " k1:" + testPubKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			if cfg, err := ParseConfig(c.app, c.url, c.keys, 12); err == nil {
				t.Fatalf("accepted a broken configuration: %+v", cfg)
			}
		})
	}
	// A ttl of zero or less means the default, not a session that expires at once.
	cfg, err := ParseConfig("todo", "https://gate.example.fr", testKid+":"+testPubKey, 0)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if cfg.SessionTTL != DefaultSessionTTL {
		t.Errorf("session ttl is %s, want the %s default", cfg.SessionTTL, DefaultSessionTTL)
	}
}

func TestAuthorizeURL(t *testing.T) {
	got := testConfig(t).AuthorizeURL("abc123")
	for _, want := range []string{"https://gate.example.fr/authorize?", "app=todo", "state=abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from %q", want, got)
		}
	}
}

// The state is what binds a ticket to the browser that started the sign-in.
func TestState(t *testing.T) {
	state, err := NewState()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if len(state) < 20 {
		t.Fatalf("a %d-character state is not 128 bits", len(state))
	}
	cookie := StateCookieValue(state, "/admin/orders/42")

	next, ok := MatchState(cookie, state)
	if !ok || next != "/admin/orders/42" {
		t.Fatalf("a matching state was refused: next=%q ok=%v", next, ok)
	}
	for _, c := range []struct{ name, cookie, got string }{
		{"a different state", cookie, "somethingelse"},
		{"no state at all", cookie, ""},
		{"an empty cookie", "", state},
		{"a cookie with no separator", state, state},
		{"a prefix of the state", cookie, state[:8]},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := MatchState(c.cookie, c.got); ok {
				t.Fatal("a mismatched state was accepted")
			}
		})
	}
	// Two states in a row must differ, or the binding is decoration.
	other, _ := NewState()
	if other == state {
		t.Fatal("the same state was minted twice")
	}
}

func TestSubjectAndFingerprint(t *testing.T) {
	if got := SubjectFor("g_abc"); got != "gate:g_abc" {
		t.Errorf("subject is %q", got)
	}
	if !IsGateSubject(SubjectFor("g_abc")) {
		t.Error("a gate subject is not recognised as one")
	}
	if IsGateSubject("admin") {
		t.Error("a local subject is taken for a gate one")
	}
	fp := CredFP("g_abc", "s_123")
	if fp != "gate-v1|g_abc|s_123" {
		t.Fatalf("fingerprint is %q — a revocation naming only the session must be able to match it", fp)
	}
	// Which is what this suffix is for.
	if !strings.HasSuffix(fp, CredSuffixForSid("s_123")) {
		t.Error("the per-session suffix does not match the fingerprint it should find")
	}
	if strings.HasSuffix(fp, CredSuffixForSid("s_999")) {
		t.Error("the suffix of another session matched")
	}
}
