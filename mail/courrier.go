package mail

// Courrier envoie une pièce au destinataire, en courriel avec le PDF joint.
// Repris de lease et picsou, avec les deux ajouts de picsou : Reply-To, pour
// qu'une réponse arrive dans une vraie boîte, et une copie cachée, pour que
// chaque envoi y laisse sa trace. Il construit le message MIME à la main — une
// centaine de lignes d'en-têtes contre une dépendance, pour une forme de message
// qui ne varie jamais.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// CourrierConfig is the relay to talk to and the address to send from.
type CourrierConfig struct {
	Host string
	Port string
	// TLS is "starttls" (the default), "ssl" (implicit TLS, port 465) or "none".
	TLS  string
	User string
	Pass string
	// From must be an address the relay accepts. On a verified domain, or the
	// message goes to spam — which looks exactly like a message that was never
	// sent.
	From string
	// ReplyTo est l'adresse où répondre, quand From est une adresse d'envoi
	// qu'on ne lit pas. Vide, pas d'en-tête.
	ReplyTo string
	// Copie reçoit chaque message en copie cachée : destinataires de
	// l'enveloppe, absents des en-têtes.
	Copie []string
}

// Piece is a file attached to the message.
type Piece struct {
	Nom     string
	Type    string // MIME type, e.g. "application/pdf"
	Contenu []byte
}

// echangeTimeout bounds the WHOLE SMTP exchange — bannière, EHLO, STARTTLS, AUTH,
// DATA, QUIT — et pas seulement la connexion : net/smtp ne pose aucune échéance de
// lui-même, si bien qu'un relais qui cesse de répondre au milieu d'un DATA laissait
// la goroutine dessus pour toujours.
const echangeTimeout = 30 * time.Second

// Courrier sends messages through one relay.
type Courrier struct {
	cfg CourrierConfig
	// now is injectable so a test can assert a stable Date header.
	now func() time.Time
}

// NewCourrier builds a sender. An unconfigured one is a no-op that reports itself
// through Enabled: the service runs perfectly well without e-mail, it just
// cannot hand a quittance over that way.
func NewCourrier(cfg CourrierConfig) *Courrier {
	if cfg.Port == "" {
		cfg.Port = "587"
	}
	if cfg.TLS == "" {
		cfg.TLS = "starttls"
	}
	return &Courrier{cfg: cfg, now: time.Now}
}

// Enabled reports whether enough is configured to send anything.
func (s *Courrier) Enabled() bool {
	return strings.TrimSpace(s.cfg.Host) != "" && strings.TrimSpace(s.cfg.From) != ""
}

// ErrDesactive is returned when nothing is configured.
var ErrDesactive = errors.New("envoi d'e-mail non configuré (SMTP_HOST, MAIL_FROM)")

// Envoyer sends one message with one attachment to one or more recipients.
func (s *Courrier) Envoyer(ctx context.Context, to []string, sujet, texte string, piece Piece) error {
	if !s.Enabled() {
		return ErrDesactive
	}
	dest := nettoyer(to)
	if len(dest) == 0 {
		return errors.New("aucun destinataire")
	}
	corps, err := s.message(dest, sujet, texte, piece)
	if err != nil {
		return err
	}
	return s.remettre(ctx, dest, corps)
}

// message builds the RFC 5322 message: a text part, then the attachment.
func (s *Courrier) message(to []string, sujet, texte string, piece Piece) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	entetes := textproto.MIMEHeader{}
	entetes.Set("From", s.cfg.From)
	entetes.Set("To", strings.Join(to, ", "))
	// Subjects carry accents — "Quittance d'août" — and a raw 8-bit header is not
	// a header. RFC 2047 is what makes it one.
	entetes.Set("Subject", mime.QEncoding.Encode("utf-8", sujet))
	entetes.Set("Date", s.now().Format(time.RFC1123Z))
	entetes.Set("MIME-Version", "1.0")
	entetes.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())

	var tete bytes.Buffer
	if s.cfg.ReplyTo != "" {
		entetes.Set("Reply-To", s.cfg.ReplyTo)
	}
	for _, k := range []string{"From", "Reply-To", "To", "Subject", "Date", "MIME-Version", "Content-Type"} {
		if entetes.Get(k) == "" {
			continue
		}
		fmt.Fprintf(&tete, "%s: %s\r\n", k, entetes.Get(k))
	}
	tete.WriteString("\r\n")

	texteHeader := textproto.MIMEHeader{}
	texteHeader.Set("Content-Type", "text/plain; charset=utf-8")
	texteHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	partie, err := mw.CreatePart(texteHeader)
	if err != nil {
		return nil, err
	}
	qp := quotedprintable.NewWriter(partie)
	if _, err := qp.Write([]byte(texte)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}

	if len(piece.Contenu) > 0 {
		typ := piece.Type
		if typ == "" {
			typ = "application/octet-stream"
		}
		pj := textproto.MIMEHeader{}
		pj.Set("Content-Type", typ+`; name="`+piece.Nom+`"`)
		pj.Set("Content-Transfer-Encoding", "base64")
		pj.Set("Content-Disposition", `attachment; filename="`+piece.Nom+`"`)
		w, err := mw.CreatePart(pj)
		if err != nil {
			return nil, err
		}
		if err := ecrireBase64(w, piece.Contenu); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return append(tete.Bytes(), buf.Bytes()...), nil
}

// remettre opens the connection the configured way and hands the message over.
//
// Chaque échec nomme L'ÉTAPE qui a cassé et LE COMPTE sur lequel elle a cassé,
// parce que la réponse du serveur, elle, ne le dit jamais : « 535 Authentication
// failed » tout seul laisse choisir entre un mauvais identifiant, un mauvais mot de
// passe, une mauvaise région et un mauvais domaine. Nommer l'identifiant refusé
// répond à trois des quatre sur le coup. Le mot de passe n'est jamais écrit (voir
// Compte). C'est la même règle que dans resa, d'où elle vient.
func (s *Courrier) remettre(ctx context.Context, to []string, corps []byte) error {
	addr := net.JoinHostPort(s.cfg.Host, s.cfg.Port)
	d := net.Dialer{Timeout: 15 * time.Second}
	echec := func(etape string, err error) error {
		return fmt.Errorf("mail : %s a échoué (%s) : %w", etape, s.Compte(), err)
	}

	var (
		conn net.Conn
		err  error
	)
	if strings.EqualFold(s.cfg.TLS, "ssl") {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return echec("la connexion", err)
	}
	defer conn.Close()
	// L'échéance porte sur tout l'échange, et non sur la seule connexion.
	_ = conn.SetDeadline(time.Now().Add(echangeTimeout))

	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return echec("la bannière", err)
	}
	defer func() { _ = c.Close() }()

	if strings.EqualFold(s.cfg.TLS, "starttls") {
		if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return echec("STARTTLS", err)
		}
	}
	if s.cfg.User != "" {
		// PlainAuth refuses an unencrypted connection by itself; the error it
		// returns is the one to read rather than one to invent here.
		if err := c.Auth(smtp.PlainAuth("", s.cfg.User, s.cfg.Pass, s.cfg.Host)); err != nil {
			return echec("l'authentification", err)
		}
	}
	expediteur, err := adresse(s.cfg.From)
	if err != nil {
		return echec("la lecture de MAIL_FROM", err)
	}
	if err := c.Mail(expediteur); err != nil {
		// L'autre moitié de la panne qu'on confond toujours : le serveur accepte
		// l'identifiant et refuse l'expéditeur, hors de son domaine vérifié.
		return echec("MAIL FROM", err)
	}
	for _, r := range nettoyer(append(append([]string(nil), to...), s.cfg.Copie...)) {
		if err := c.Rcpt(r); err != nil {
			return echec("RCPT TO "+r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return echec("DATA", err)
	}
	if _, err := w.Write(corps); err != nil {
		return echec("l'écriture du message", err)
	}
	if err := w.Close(); err != nil {
		return echec("la fermeture du message", err)
	}
	if err := c.Quit(); err != nil {
		return echec("QUIT", err)
	}
	return nil
}

// Compte describes the account a message travels through, for a log line: the
// server and its transport, the login it authenticates with, and the address it is
// sent as. Tout sauf le mot de passe — cette chaîne part dans les journaux, qui se
// recopient dans une conversation.
//
// C'est la moitié habituelle de la panne : un serveur qui accepte l'identifiant
// refuse encore une adresse hors de son domaine vérifié, et voir les deux côte à
// côte répond à la question sans essayer.
func (s *Courrier) Compte() string {
	login := s.cfg.User
	if login == "" {
		login = "<aucun>"
	}
	return fmt.Sprintf("host=%s tls=%s user=%s from=%s",
		net.JoinHostPort(s.cfg.Host, s.cfg.Port), s.modeTLS(), login, s.cfg.From)
}

// modeTLS names the transport as it is actually used, resolving the empty and the
// unrecognised value to the "starttls" default they behave as.
func (s *Courrier) modeTLS() string {
	mode := strings.ToLower(strings.TrimSpace(s.cfg.TLS))
	if mode == "ssl" || mode == "none" {
		return mode
	}
	return "starttls"
}

// ecrireBase64 writes the attachment in 76-character lines, as the format wants:
// a base64 body on one endless line is legal by the letter and rejected or
// mangled by enough relays to be worth avoiding.
func ecrireBase64(w io.Writer, data []byte) error {
	encode := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(encode); i += 76 {
		if _, err := io.WriteString(w, encode[i:min(i+76, len(encode))]+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// adresse extracts the bare address from a possibly decorated From.
func adresse(s string) (string, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		// A bare "user@host" without a display name parses; anything else is a
		// configuration mistake worth naming.
		return "", err
	}
	return a.Address, nil
}

// nettoyer drops the empty recipients — a tenant with no e-mail address on file —
// and de-duplicates a couple who share one.
func nettoyer(to []string) []string {
	vus := make(map[string]bool, len(to))
	out := make([]string, 0, len(to))
	for _, a := range to {
		a = strings.TrimSpace(a)
		if a == "" || vus[a] {
			continue
		}
		vus[a] = true
		out = append(out, a)
	}
	return out
}
