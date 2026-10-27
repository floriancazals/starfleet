package mail

import (
	"bytes"
	b64std "encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// Un message avec pièces jointes doit se relire avec la bibliothèque standard :
// c'est ce que fera Gmail, et une frontière mal posée donne un courriel dont les
// photos sont devenues du texte illisible au milieu du corps.
func TestMessageAvecPiecesJointesSeRelit(t *testing.T) {
	photo := bytes.Repeat([]byte{0xFF, 0xD8, 0x42}, 400)
	brut := buildMessage("site@exemple.fr", "Site TPAG", "client@exemple.fr", "guillaume@exemple.fr",
		"Devis : terrassement — Baziège", "Bonjour,\nune demande.", "<p>Bonjour</p>",
		[]Attachment{{Name: "photo-1.jpg", ContentType: "image/jpeg", Data: photo}})

	msg, err := mail.ReadMessage(bytes.NewReader(brut))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Reply-To"); got != "client@exemple.fr" {
		t.Fatalf("Reply-To = %q", got)
	}
	sujet, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || sujet != "Devis : terrassement — Baziège" {
		t.Fatalf("sujet = %q (%v)", sujet, err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("type = %q (%v)", mt, err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])

	corps, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct := corps.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/alternative") {
		t.Fatalf("première partie = %q, attendu le corps texte+html", ct)
	}

	pj, err := r.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if pj.FileName() != "photo-1.jpg" {
		t.Fatalf("nom de fichier = %q", pj.FileName())
	}
	// multipart.Reader décode lui-même le quoted-printable, pas le base64 : on le
	// fait à la main, comme le ferait un client de messagerie.
	enc, _ := io.ReadAll(pj)
	dec, err := decode64(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, photo) {
		t.Fatal("la photo ne revient pas identique")
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Fatalf("une partie de trop : %v", err)
	}
}

func TestSansPieceJointeLeMessageResteSimple(t *testing.T) {
	brut := string(buildMessage("a@exemple.fr", "", "", "b@exemple.fr", "Sujet", "Corps", "", nil))
	if !strings.Contains(brut, "Content-Type: text/plain; charset=UTF-8") || strings.Contains(brut, "multipart") {
		t.Fatalf("message inattendu :\n%s", brut)
	}
}

func TestUnSujetSurDeuxLignesEstRefuse(t *testing.T) {
	m := New("smtp.exemple.fr", "587", "", "", "", "site@exemple.fr")
	err := m.SendWith(Sender{}, "b@exemple.fr", "Devis\r\nBcc: x@exemple.fr", "corps", "", nil)
	if err == nil || !strings.Contains(err.Error(), "line break") {
		t.Fatalf("err = %v, attendu un refus avant toute connexion", err)
	}
}

func decode64(b []byte) ([]byte, error) {
	return base64Decode(strings.NewReplacer("\r", "", "\n", "").Replace(string(b)))
}

func base64Decode(s string) ([]byte, error) { return b64std.StdEncoding.DecodeString(s) }
