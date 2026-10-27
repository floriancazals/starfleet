package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func expediteur(t *testing.T) *Courrier {
	t.Helper()
	s := NewCourrier(CourrierConfig{Host: "relais.example", From: "bailleur@example.fr", TLS: "none"})
	s.now = func() time.Time { return time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC) }
	return s
}

// Le message doit être un vrai message : des en-têtes analysables, un sujet
// encodé malgré les accents, et la pièce jointe qui se redécode à l'octet.
func TestMessageEstUnVraiMessage(t *testing.T) {
	s := expediteur(t)
	pdf := []byte("%PDF-1.3 quittance de mars, avec des accents : été")
	brut, err := s.message([]string{"marie@example.fr", "paul@example.fr"},
		"Votre quittance d'août 2026", "Bonjour,\n\nVoici votre quittance.\n", Piece{
			Nom: "quittance-2026-08.pdf", Type: "application/pdf", Contenu: pdf,
		})
	if err != nil {
		t.Fatalf("message: %v", err)
	}

	msg, err := mail.ReadMessage(strings.NewReader(string(brut)))
	if err != nil {
		t.Fatalf("le message ne s'analyse pas : %v", err)
	}
	if got := msg.Header.Get("To"); got != "marie@example.fr, paul@example.fr" {
		t.Errorf("To = %q", got)
	}
	// Le sujet voyage encodé, et se redécode exactement.
	sujet, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("sujet illisible : %v", err)
	}
	if sujet != "Votre quittance d'août 2026" {
		t.Errorf("sujet = %q", sujet)
	}
	if strings.Contains(msg.Header.Get("Subject"), "août") {
		t.Error("le sujet part en 8 bits brut : ce n'est pas un en-tête valide")
	}
	if _, err := mail.ParseDate(msg.Header.Get("Date")); err != nil {
		t.Errorf("Date illisible : %v", err)
	}

	typ, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || typ != "multipart/mixed" {
		t.Fatalf("Content-Type = %q, %v", typ, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])

	// Première partie : le texte.
	texte, err := mr.NextPart()
	if err != nil {
		t.Fatalf("partie texte : %v", err)
	}
	corps, err := io.ReadAll(texte)
	if err != nil {
		t.Fatalf("lecture du texte : %v", err)
	}
	if !strings.Contains(string(corps), "Voici votre quittance") {
		t.Errorf("texte = %q", corps)
	}

	// Seconde partie : la pièce jointe, qui doit revenir identique.
	pj, err := mr.NextPart()
	if err != nil {
		t.Fatalf("pièce jointe : %v", err)
	}
	if got := pj.FileName(); got != "quittance-2026-08.pdf" {
		t.Errorf("nom de la pièce = %q", got)
	}
	if got := pj.Header.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Errorf("encodage = %q", got)
	}
	encode, err := io.ReadAll(pj)
	if err != nil {
		t.Fatalf("lecture de la pièce : %v", err)
	}
	decode, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(
		strings.ReplaceAll(string(encode), "\r\n", ""), "\n", ""))
	if err != nil {
		t.Fatalf("base64 illisible : %v", err)
	}
	if string(decode) != string(pdf) {
		t.Error("la pièce jointe ne revient pas identique")
	}
}

// Aucune ligne du message ne doit dépasser 998 octets : c'est la limite de la
// RFC, et un base64 sur une seule ligne interminable se fait couper par assez de
// relais pour que ça compte.
func TestLignesBornees(t *testing.T) {
	s := expediteur(t)
	gros := make([]byte, 40_000)
	for i := range gros {
		gros[i] = byte('A' + i%26)
	}
	brut, err := s.message([]string{"marie@example.fr"}, "Sujet", "Texte", Piece{
		Nom: "gros.pdf", Type: "application/pdf", Contenu: gros,
	})
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	for i, l := range strings.Split(string(brut), "\r\n") {
		if len(l) > 998 {
			t.Fatalf("ligne %d longue de %d octets", i, len(l))
		}
	}
}

func TestEnabledEtDestinataires(t *testing.T) {
	if NewCourrier(CourrierConfig{}).Enabled() {
		t.Error("un expéditeur vide ne devrait pas être actif")
	}
	if NewCourrier(CourrierConfig{Host: "relais"}).Enabled() {
		t.Error("sans MAIL_FROM, pas d'envoi")
	}
	if !NewCourrier(CourrierConfig{Host: "relais", From: "a@b.fr"}).Enabled() {
		t.Error("host + from devrait suffire")
	}
	// Envoyer sans configuration dit pourquoi, plutôt que d'échouer en silence.
	if err := NewCourrier(CourrierConfig{}).Envoyer(context.Background(), []string{"a@b.fr"}, "s", "t", Piece{}); err == nil {
		t.Error("Envoyer sans configuration devrait échouer")
	}
	// Un locataire sans adresse ne fait pas planter l'envoi à l'autre, et deux
	// signataires qui partagent une boîte ne la reçoivent pas deux fois.
	got := nettoyer([]string{" marie@example.fr ", "", "marie@example.fr", "paul@example.fr", "   "})
	if len(got) != 2 || got[0] != "marie@example.fr" || got[1] != "paul@example.fr" {
		t.Errorf("nettoyer = %v", got)
	}
	s := expediteur(t)
	if err := s.Envoyer(context.Background(), []string{"", "  "}, "s", "t", Piece{}); err == nil {
		t.Error("aucun destinataire utilisable devrait échouer")
	}
}

// Le tour complet contre un relais bidon : ce qui compte est que les octets qui
// passent sur le fil soient bien le message.
func TestRemettre(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	recu := make(chan string, 1)
	go relaisBidon(t, ln, recu)

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s := NewCourrier(CourrierConfig{Host: host, Port: port, TLS: "none", From: "bailleur@example.fr", ReplyTo: "moi@example.fr", Copie: []string{"moi@example.fr"}})
	s.now = func() time.Time { return time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC) }

	err = s.Envoyer(context.Background(), []string{"marie@example.fr"},
		"Quittance de mars 2026", "Bonjour,\n\nVoici votre quittance.\n",
		Piece{Nom: "quittance-2026-03.pdf", Type: "application/pdf", Contenu: []byte("%PDF-1.3 x")})
	if err != nil {
		t.Fatalf("Envoyer: %v", err)
	}
	select {
	case conversation := <-recu:
		for _, attendu := range []string{
			"MAIL FROM:<bailleur@example.fr>",
			"RCPT TO:<marie@example.fr>",
			"RCPT TO:<moi@example.fr>",
			"Reply-To: moi@example.fr",
			"Subject: ",
			"quittance-2026-03.pdf",
		} {
			if !strings.Contains(conversation, attendu) {
				t.Errorf("la conversation ne contient pas %q", attendu)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("le relais bidon n'a rien reçu")
	}
}

// relaisBidon parle juste assez de SMTP pour accepter un message et le rendre.
func relaisBidon(t *testing.T, ln net.Listener, recu chan<- string) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	var tout strings.Builder
	ecrire := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }

	ecrire("220 relais bidon")
	dansData := false
	for {
		ligne, err := r.ReadString('\n')
		if err != nil {
			break
		}
		tout.WriteString(ligne)
		l := strings.TrimRight(ligne, "\r\n")
		if dansData {
			if l == "." {
				dansData = false
				ecrire("250 OK")
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "EHLO"), strings.HasPrefix(l, "HELO"):
			ecrire("250-relais bidon")
			ecrire("250 SIZE 35882577")
		case strings.HasPrefix(l, "MAIL FROM"), strings.HasPrefix(l, "RCPT TO"):
			ecrire("250 OK")
		case l == "DATA":
			dansData = true
			ecrire("354 go ahead")
		case l == "QUIT":
			ecrire("221 bye")
			recu <- tout.String()
			return
		default:
			ecrire("250 OK")
		}
	}
	recu <- tout.String()
}
