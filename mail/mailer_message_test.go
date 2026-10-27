package mail

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBuildMessage(t *testing.T) {
	msg := string(buildMessage("studio@example.com", "", "", "alice@example.com",
		"Rendez-vous confirmé — Kiné", "Bonjour Alice,\nÀ demain.\n", "", nil))

	for _, want := range []string{
		"From: studio@example.com\r\n",
		"To: alice@example.com\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing header %q", want)
		}
	}
	// Subject with an accent must be Q-encoded, not raw UTF-8.
	if strings.Contains(msg, "Rendez-vous confirmé — Kiné\r\n") {
		t.Error("subject should be RFC 2047 encoded, not raw")
	}
	if !strings.Contains(msg, "Subject: =?utf-8?q?") {
		t.Error("subject should be Q-encoded")
	}
	// Header/body separation and CRLF body.
	if !strings.Contains(msg, "\r\n\r\nBonjour Alice,\r\nÀ demain.\r\n") {
		t.Errorf("body not CRLF-normalised or missing:\n%q", msg)
	}
	// No identity of its own: no Reply-To at all, rather than one pointing at the
	// sender (which would be a header saying nothing).
	if strings.Contains(msg, "Reply-To:") {
		t.Errorf("a sender with no reply address must not carry Reply-To:\n%q", msg)
	}
}

// With a styled alternative the message becomes multipart/alternative, carrying
// both bodies: the plain text a text-only client reads, and the HTML the others
// show. Order matters — a client renders the last part it understands, so the
// text must come first or nobody would ever see the card.
func TestBuildMessageMultipart(t *testing.T) {
	html := "<!doctype html><html><body><p>" + strings.Repeat("Bonjour Alice, ", 20) + "</p></body></html>"
	msg := string(buildMessage("studio@example.com", "", "", "alice@example.com",
		"Rendez-vous confirmé", "Bonjour Alice,\nÀ demain.\n", html, nil))

	if !strings.Contains(msg, `Content-Type: multipart/alternative; boundary="`) {
		t.Fatalf("not a multipart message:\n%q", msg)
	}
	_, body, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body separation:\n%q", msg)
	}
	text := strings.Index(body, "Content-Type: text/plain; charset=UTF-8")
	rich := strings.Index(body, "Content-Type: text/html; charset=UTF-8")
	switch {
	case text < 0 || rich < 0:
		t.Fatalf("a part is missing:\n%q", body)
	case rich < text:
		t.Errorf("the HTML part must come after the text one:\n%q", body)
	}
	// The text part travels as it was written; the HTML one is quoted-printable,
	// so no line of it can break the 998-octet limit of a strict relay.
	if !strings.Contains(body, "Bonjour Alice,\r\nÀ demain.\r\n") {
		t.Errorf("the text body is not intact:\n%q", body)
	}
	if !strings.Contains(body, "Content-Transfer-Encoding: quoted-printable") {
		t.Errorf("the HTML part is not encoded:\n%q", body)
	}
	for _, line := range strings.Split(body, "\r\n") {
		if len(line) > 998 {
			t.Errorf("a line exceeds the SMTP limit (%d octets): %q", len(line), line)
		}
	}
	// The closing delimiter: a truncated multipart body is shown as source by some
	// clients and rejected by others.
	if !strings.HasSuffix(strings.TrimRight(body, "\r\n"), "--") {
		t.Errorf("the multipart body is not closed:\n%q", body)
	}
}

// An identity writes its own name, address and reply address into the headers —
// what lets the cabinet and the lodging share one SMTP account without the
// customer ever seeing the wrong one.
func TestBuildMessageWithIdentity(t *testing.T) {
	msg := string(buildMessage("escale@mg.example.fr", "L'Escale du Midi", "flora@example.fr",
		"alice@example.com", "Séjour confirmé", "Bonjour,\n", "", nil))

	if !strings.Contains(msg, "Reply-To: flora@example.fr\r\n") {
		t.Errorf("missing Reply-To:\n%q", msg)
	}
	// An ASCII display name is a quoted string, so any punctuation it carries
	// ("Dr. Martin", "Cabinet, kiné") stays a valid RFC 5322 phrase.
	if !strings.Contains(msg, `From: "L'Escale du Midi" <escale@mg.example.fr>`+"\r\n") {
		t.Errorf("From header wrong:\n%q", msg)
	}
}

// A display name with an accent cannot travel raw: it becomes an RFC 2047 encoded
// word, which must not be quoted (a quoted encoded word is read literally).
func TestBuildMessageEncodesNonASCIIName(t *testing.T) {
	msg := string(buildMessage("kine@mg.example.fr", "Cabinet — kiné", "",
		"alice@example.com", "Hi", "body", "", nil))

	if !strings.Contains(msg, "From: =?utf-8?q?") {
		t.Errorf("the accented display name is not Q-encoded:\n%q", msg)
	}
	if strings.Contains(msg, `From: "=?utf-8?q?`) {
		t.Errorf("an encoded word must not be quoted:\n%q", msg)
	}
	if !strings.Contains(msg, "<kine@mg.example.fr>\r\n") {
		t.Errorf("the From header must carry the identity's address:\n%q", msg)
	}
}

// A quote inside the name is escaped rather than closing the quoted string early.
func TestBuildMessageEscapesQuoteInName(t *testing.T) {
	msg := string(buildMessage("a@b.fr", `Studio "Le Midi"`, "", "alice@example.com", "Hi", "body", "", nil))
	if !strings.Contains(msg, `From: "Studio \"Le Midi\"" <a@b.fr>`+"\r\n") {
		t.Errorf("quote not escaped in the display name:\n%q", msg)
	}
}

// A refused login must be diagnosable from the log line alone: which step broke,
// on which server, with which login and as which sender. "535 Authentication
// failed" on its own leaves a host guessing between a wrong login, a wrong
// password, a wrong region and a wrong domain.
func TestSendAsErrorNamesTheAccount(t *testing.T) {
	host, port := refusingSMTP(t, "535 Authentication failed")
	m := New(host, port, "none", "postmaster@mg.example.com", "s3cret-pass", "booking@mg.example.fr")

	err := m.SendAs(Sender{}, "alice@example.com", "Hi", "body", "")
	if err == nil {
		t.Fatal("a refused login must be an error")
	}
	for _, want := range []string{
		"auth failed", // the step, not just the server's number
		// The server's own answer, kept — code and words asserted apart because
		// how the two are joined belongs to net/textproto, which quotes the
		// message since Go 1.26.4 (`535 "Authentication failed"`).
		"535",
		"Authentication failed",
		host + ":" + port, // where
		"user=postmaster@mg.example.com",
		"from=booking@mg.example.fr", // the other half of the usual mistake
		"tls=none",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
	// The one thing a log must never carry.
	if strings.Contains(err.Error(), "s3cret-pass") {
		t.Errorf("the password leaked into the error:\n%v", err)
	}
}

// A step that is not the login names itself too, so a sender the server refuses is
// never mistaken for a credential problem.
func TestSendAsErrorNamesTheStep(t *testing.T) {
	host, port := refusingSMTP(t, "553 Sender address rejected")
	m := New(host, port, "none", "", "", "booking@elsewhere.example")

	err := m.SendAs(Sender{}, "alice@example.com", "Hi", "body", "")
	if err == nil || !strings.Contains(err.Error(), "MAIL FROM failed") {
		t.Errorf("a refused sender should name MAIL FROM, got: %v", err)
	}
	if !strings.Contains(err.Error(), "user=<none>") {
		t.Errorf("an account with no login should say so:\n%v", err)
	}
}

// refusingSMTP answers the greeting and EHLO (advertising AUTH, so the client
// tries to log in) and then refuses everything else with reply. It is the shape of
// a provider turning down a credential or a sender.
func refusingSMTP(t *testing.T, reply string) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by the cleanup
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				write := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				write("220 fake ESMTP")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						write("250-fake")
						write("250 AUTH PLAIN LOGIN")
					case strings.HasPrefix(cmd, "QUIT"):
						write("221 bye")
						return
					default:
						write(reply)
					}
				}
			}()
		}
	}()
	return host, port
}

func TestSendAsRejectsHeaderInjection(t *testing.T) {
	m := New("smtp.example.com", "587", "starttls", "", "", "from@example.com")
	// Every header value an admin can type is checked, not just the recipient: a
	// sender or a reply address carrying a CRLF would smuggle extra headers.
	for _, s := range []Sender{
		{Address: "escale@example.com\r\nBcc: spam@example.com"},
		{Address: "escale@example.com", ReplyTo: "x@example.com\r\nBcc: spam@example.com"},
		{Address: "escale@example.com", Name: "Escale\r\nBcc: spam@example.com"},
	} {
		err := m.SendAs(s, "alice@example.com", "Hi", "body", "")
		if err == nil || err == ErrNotConfigured {
			t.Errorf("CRLF in %+v should be refused, got %v", s, err)
		}
	}
}

// A sender with no address of its own falls back to the configured MAIL_FROM
// rather than sending from nobody.
func TestSendAsFallsBackToConfiguredFrom(t *testing.T) {
	msg := string(buildMessage("from@example.com", "", "", "alice@example.com", "Hi", "body", "", nil))
	if !strings.Contains(msg, "From: from@example.com\r\n") {
		t.Errorf("fallback From missing:\n%q", msg)
	}
}

func TestEnabled(t *testing.T) {
	if New("", "587", "starttls", "", "", "from@x").Enabled() {
		t.Error("no host → disabled")
	}
	if New("smtp.x", "587", "starttls", "", "", "").Enabled() {
		t.Error("no from → disabled")
	}
	if !New("smtp.x", "587", "starttls", "", "", "from@x").Enabled() {
		t.Error("host+from → enabled")
	}
	var nilM *Mailer
	if nilM.Enabled() {
		t.Error("nil mailer → disabled")
	}
	if err := nilM.Send("a@b", "s", "b"); err != ErrNotConfigured {
		t.Errorf("nil Send = %v, want ErrNotConfigured", err)
	}
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	m := New("smtp.example.com", "587", "starttls", "", "", "from@example.com")
	// A recipient carrying a CRLF must be refused before any SMTP dial, so no
	// extra header (e.g. Bcc) can be smuggled in.
	err := m.Send("victim@example.com\r\nBcc: spam@example.com", "Hi", "body")
	if err == nil || err == ErrNotConfigured {
		t.Fatalf("CRLF recipient should be refused, got %v", err)
	}
}
