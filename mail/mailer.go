// Package mail envoie les courriels de la flotte par SMTP, avec la seule
// bibliothèque standard. Deux formes, qui étaient deux familles de copies :
//
//   - Mailer (folio, tpag, enseigne, lebonvoisin, resa) : un message à un
//     destinataire, en texte avec une alternative HTML facultative, des pièces
//     jointes, et l'identité d'expéditeur choisie à chaque envoi (Sender) ;
//   - Courrier (lease, picsou) : une pièce — une quittance, une facture — jointe
//     à un texte, envoyée à plusieurs destinataires avec une copie cachée.
//
// Trois transports : TLS implicite (ssl, port 465), STARTTLS (le défaut, 587)
// et en clair (none). smtp.PlainAuth refuse de donner un mot de passe à un
// serveur qui n'est pas sur une connexion chiffrée (localhost excepté), et c'est
// une qualité : un mot de passe envoyé en clair à un relais vaut plus, pour un
// attaquant, que la boîte elle-même.
package mail

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// dialTimeout bounds the TCP/TLS dial; mailDeadline bounds the whole SMTP
// conversation after connect. Together they guarantee Send always returns —
// critical because the delivery loop calls it synchronously and a server that
// accepts the socket then stalls (black-holed relay, half-open connection) must
// never wedge the loop or block shutdown.
const (
	dialTimeout  = 10 * time.Second
	mailDeadline = 60 * time.Second // a few photos make a message of 1–2 MB
)

// ErrNotConfigured is returned by Send when no SMTP host / from address is set.
var ErrNotConfigured = errors.New("mail: SMTP not configured")

// Mailer sends UTF-8 messages from a single account. The zero value is unusable;
// build one with New. A nil *Mailer is a valid, disabled mailer (Enabled reports
// false, Send returns ErrNotConfigured).
type Mailer struct {
	host, port, tls, user, pass, from string
}

// New builds a Mailer. tls is one of "starttls" (default), "ssl" or "none";
// anything unrecognised is treated as "starttls".
func New(host, port, tlsMode, user, pass, from string) *Mailer {
	return &Mailer{
		host: host,
		port: port,
		tls:  strings.ToLower(strings.TrimSpace(tlsMode)),
		user: user,
		pass: pass,
		from: from,
	}
}

// Enabled reports whether the mailer can send (host and from address set).
func (m *Mailer) Enabled() bool {
	return m != nil && m.host != "" && m.from != ""
}

// Sender is the identity a message is written under: the name and address the
// recipient reads, and where a reply should land. The zero value means "the
// mailer's own configured address" (MAIL_FROM).
//
// Address must be one the SMTP account is allowed to send as — the account itself
// never changes, only the identity.
type Sender struct {
	Name    string // display name, e.g. "Site TPAG" ("" = address only)
	Address string // "" = the mailer's configured MAIL_FROM
	ReplyTo string // "" = replies go to Address
}

// Send delivers a UTF-8 text/plain message from the mailer's configured address.
func (m *Mailer) Send(to, subject, body string) error {
	return m.SendAs(Sender{}, to, subject, body, "")
}

// Attachment is a file carried by a message.
type Attachment struct {
	Name        string // as the recipient sees it, e.g. "photo-1.jpg"
	ContentType string // e.g. "image/jpeg"
	Data        []byte
}

// SendAs delivers a UTF-8 message to a single recipient under the given identity.
// text is the plain-text body; html is the styled alternative, and "" sends the
// text alone. It returns ErrNotConfigured when the mailer is disabled.
func (m *Mailer) SendAs(s Sender, to, subject, text, html string) error {
	return m.SendWith(s, to, subject, text, html, nil)
}

// SendWith is SendAs plus attachments.
func (m *Mailer) SendWith(s Sender, to, subject, text, html string, files []Attachment) error {
	if !m.Enabled() {
		return ErrNotConfigured
	}
	from := strings.TrimSpace(s.Address)
	if from == "" {
		from = m.from
	}
	// Defence in depth against e-mail header injection: a header value must be a
	// single line. Recipients are validated upstream, but a stored bad address —
	// or a sender an admin typed into the book — must never smuggle extra headers.
	for _, v := range []string{to, from, s.ReplyTo, s.Name, subject} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("mail: header value contains a line break: %q", v)
		}
	}
	for _, f := range files {
		if strings.ContainsAny(f.Name+f.ContentType, "\r\n\"") {
			return fmt.Errorf("mail: attachment name contains a forbidden character: %q", f.Name)
		}
	}
	msg := buildMessage(from, s.Name, s.ReplyTo, to, subject, text, html, files)
	addr := net.JoinHostPort(m.host, m.port)
	// Every failure below says which step of the conversation broke and on which
	// account, because the server's own answer never does. "535 Authentication
	// failed" alone leaves a host guessing between a wrong login, a wrong
	// password, a wrong region and a wrong domain; naming the login it was
	// refused for answers three of the four on the spot. The password is the one
	// thing never written — see Account.
	fail := func(step string, err error) error {
		return fmt.Errorf("mail: %s failed (%s): %w", step, m.Account(from), err)
	}

	var auth smtp.Auth
	if m.user != "" {
		auth = smtp.PlainAuth("", m.user, m.pass, m.host)
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	var conn net.Conn
	var err error
	if m.tls == "ssl" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: m.host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fail("dial", err)
	}
	// Bound the whole exchange (banner, EHLO, STARTTLS, AUTH, DATA, QUIT), not
	// just the dial: net/smtp sets no deadlines of its own.
	_ = conn.SetDeadline(time.Now().Add(mailDeadline))

	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		_ = conn.Close()
		return fail("greeting", err)
	}
	defer func() { _ = c.Close() }()

	// STARTTLS (the default) upgrades a plaintext connection when the server
	// advertises it; "none" and "ssl" skip this step.
	if m.tls != "ssl" && m.tls != "none" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: m.host}); err != nil {
				return fail("starttls", err)
			}
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(auth); err != nil {
				return fail("auth", err)
			}
		}
	}
	// The envelope sender is the identity's address too, not just the header: SPF
	// is checked on the envelope, so letting the two diverge is what gets a
	// perfectly legitimate mail filed as spam.
	if err := c.Mail(from); err != nil {
		return fail("MAIL FROM", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fail("RCPT TO "+to, err)
	}
	wc, err := c.Data()
	if err != nil {
		return fail("DATA", err)
	}
	if _, err := wc.Write(msg); err != nil {
		return fail("writing the message", err)
	}
	if err := wc.Close(); err != nil {
		return fail("closing the message", err)
	}
	if err := c.Quit(); err != nil {
		return fail("quit", err)
	}
	return nil
}

// Account describes the account a message travels through, for a log line: the
// server and transport, the login it authenticates with, and the address it is
// sent as. Deliberately everything but the password — this string goes into logs,
// which get pasted into chats and issues.
//
// from is the envelope sender of one message ("" for the mailer's own address):
// the other half of the usual misconfiguration, since a server that accepts the
// login still refuses an address outside its verified domain.
func (m *Mailer) Account(from string) string {
	login := m.user
	if login == "" {
		login = "<none>"
	}
	if from == "" {
		from = m.from
	}
	return fmt.Sprintf("host=%s tls=%s user=%s from=%s",
		net.JoinHostPort(m.host, m.port), m.tlsMode(), login, from)
}

// tlsMode names the transport as it is actually used, resolving the empty and the
// unrecognised value to the "starttls" default they behave as.
func (m *Mailer) tlsMode() string {
	if m.tls == "ssl" || m.tls == "none" {
		return m.tls
	}
	return "starttls"
}

// buildMessage renders the RFC 822 message: headers, then the body. The Subject
// and the sender's display name are RFC 2047 Q-encoded so accents survive; line
// endings are normalised to CRLF as SMTP requires. replyTo adds a Reply-To header
// when set — what lets a mail be sent from a domain with no inbox and still be
// answerable.
//
// With an html alternative the body becomes multipart/alternative; without one it
// stays a bare text/plain, which is what a message with nothing to style wants.
// Attachments wrap whichever of the two in a multipart/mixed.
func buildMessage(from, name, replyTo, to, subject, text, html string, files []Attachment) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", fromHeader(from, name))
	fmt.Fprintf(&b, "To: %s\r\n", to)
	if replyTo = strings.TrimSpace(replyTo); replyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", replyTo)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	headers, body := buildBody(text, html)
	if len(files) == 0 {
		b.WriteString(headers)
		b.WriteString("\r\n")
		b.WriteString(body)
		return []byte(b.String())
	}
	// The body becomes the first part of a multipart/mixed, the files the next
	// ones. None of these writes can fail — the destination is a strings.Builder.
	var parts strings.Builder
	w := multipart.NewWriter(&parts)
	hdr := textproto.MIMEHeader{}
	for _, line := range strings.Split(strings.TrimSuffix(headers, "\r\n"), "\r\n") {
		if k, v, ok := strings.Cut(line, ": "); ok {
			hdr.Set(k, v)
		}
	}
	p, _ := w.CreatePart(hdr)
	_, _ = p.Write([]byte(body))
	for _, f := range files {
		ct := f.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		p, _ := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ct + "; name=\"" + f.Name + "\""},
			"Content-Disposition":       {"attachment; filename=\"" + f.Name + "\""},
			"Content-Transfer-Encoding": {"base64"},
		})
		_, _ = p.Write(base64Lines(f.Data))
	}
	_ = w.Close()
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", w.Boundary())
	b.WriteString("\r\n")
	b.WriteString(parts.String())
	return []byte(b.String())
}

// buildBody renders the body alone: its own Content-* headers (CRLF-terminated
// lines) and its content.
func buildBody(text, html string) (headers, body string) {
	if html == "" {
		return "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n", toCRLF(text)
	}
	var parts strings.Builder
	w := multipart.NewWriter(&parts)
	// Plain text first, HTML second: a client renders the *last* part it
	// understands, so this order is what decides who reads which body.
	_ = writePart(w, "text/plain; charset=UTF-8", text, false)
	_ = writePart(w, "text/html; charset=UTF-8", html, true)
	_ = w.Close()
	return fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q\r\n", w.Boundary()), parts.String()
}

// base64Lines encodes data in base64 cut into 76-character lines, the limit
// RFC 2045 sets for a transfer-encoded body.
func base64Lines(data []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	var out bytes.Buffer
	for len(enc) > 76 {
		out.WriteString(enc[:76])
		out.WriteString("\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc)
	out.WriteString("\r\n")
	return out.Bytes()
}

// writePart adds one body part. qp asks for quoted-printable, which the HTML part
// needs and the text one does not: HTML lines are long enough to break the
// 998-octet limit relays enforce, where a plain-text body never is — and left raw
// it stays readable to whoever reads the source.
func writePart(w *multipart.Writer, contentType, body string, qp bool) error {
	encoding := "8bit"
	if qp {
		encoding = "quoted-printable"
	}
	p, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {contentType},
		"Content-Transfer-Encoding": {encoding},
	})
	if err != nil {
		return err
	}
	if !qp {
		_, err = p.Write([]byte(toCRLF(body)))
		return err
	}
	enc := quotedprintable.NewWriter(p)
	if _, err := enc.Write([]byte(toCRLF(body))); err != nil {
		return err
	}
	return enc.Close()
}

// fromHeader renders the From value: `Name <address>` when the identity has a
// display name, the bare address otherwise. The name is free text an admin typed,
// so it gets whichever RFC 5322 form is valid for it:
//
//   - non-ASCII ("Cabinet — kiné") → an RFC 2047 encoded word, which must NOT be
//     quoted;
//   - plain ASCII → a quoted string, valid whatever punctuation it carries
//     ("Dr. Martin" would be an invalid bare phrase, since "." is not atext).
func fromHeader(address, name string) string {
	if name = strings.TrimSpace(name); name == "" {
		return address
	}
	if encoded := mime.QEncoding.Encode("utf-8", name); encoded != name {
		return fmt.Sprintf("%s <%s>", encoded, address)
	}
	// Escaped by hand rather than with %q, which would escape the backslashes it
	// just added a second time.
	quoted := strings.ReplaceAll(name, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	return `"` + quoted + `" <` + address + `>`
}

// toCRLF normalises any mix of line endings to CRLF.
func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
