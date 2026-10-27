package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// recu est ce qu'un faux Telegram a reçu.
type recu struct {
	path, chat, text, parseMode, filename string
	data                                  []byte
}

// fauxTelegram démarre un serveur qui note chaque requête et répond status.
func fauxTelegram(t *testing.T, status int) (*httptest.Server, func() []recu) {
	t.Helper()
	var mu sync.Mutex
	var tous []recu
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got recu
		got.path = r.URL.Path
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("envoi de fichier qui n'est pas en multipart : %v", err)
			}
			got.chat = r.FormValue("chat_id")
			if f, hdr, err := r.FormFile("document"); err == nil {
				got.filename = hdr.Filename
				got.data, _ = io.ReadAll(f)
				_ = f.Close()
			}
		} else {
			_ = r.ParseForm()
			got.chat, got.text, got.parseMode = r.PostFormValue("chat_id"), r.PostFormValue("text"), r.PostFormValue("parse_mode")
		}
		mu.Lock()
		tous = append(tous, got)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recu {
		mu.Lock()
		defer mu.Unlock()
		return append([]recu(nil), tous...)
	}
}

func viser(b *Bot, api string) { b.api = api }

func TestBotSendEnvoieDuTexteBrut(t *testing.T) {
	srv, recus := fauxTelegram(t, http.StatusOK)
	b := NewBot("123:abc")
	viser(b, srv.URL)

	if err := b.Send(context.Background(), "111", "a < b"); err != nil {
		t.Fatal(err)
	}
	got := recus()
	if len(got) != 1 {
		t.Fatalf("%d requêtes, attendu 1", len(got))
	}
	if got[0].path != "/bot123:abc/sendMessage" || got[0].chat != "111" || got[0].text != "a < b" {
		t.Errorf("reçu %+v", got[0])
	}
	if got[0].parseMode != "" {
		t.Errorf("parse_mode = %q : un texte brut ne doit pas être lu comme du HTML", got[0].parseMode)
	}
}

func TestBotSendHTMLDemandeLeModeHTML(t *testing.T) {
	srv, recus := fauxTelegram(t, http.StatusOK)
	b := NewBot("123:abc")
	viser(b, srv.URL)

	if err := b.SendHTML(context.Background(), "111", "<b>gras</b>"); err != nil {
		t.Fatal(err)
	}
	if got := recus(); got[0].parseMode != "HTML" || got[0].text != "<b>gras</b>" {
		t.Errorf("reçu %+v", got[0])
	}
}

// Un envoi sans configuration est une erreur, pas un succès muet : un rappel ne
// doit jamais être marqué envoyé alors que rien n'est parti.
func TestEteintRefuseDEnvoyer(t *testing.T) {
	ctx := context.Background()
	var nilBot *Bot
	var nilTG *Telegram
	var nilDepot *Depot
	cas := map[string]error{
		"bot nil":             nilBot.Send(ctx, "1", "x"),
		"bot sans jeton":      NewBot("").Send(ctx, "1", "x"),
		"bot sans salon":      NewBot("123:abc").SendHTML(ctx, "", "x"),
		"telegram nil":        nilTG.Send(ctx, "x"),
		"telegram sans salon": NewTelegram("123:abc", " , ").Send(ctx, "x"),
		"telegram sans jeton": NewTelegram("", "1").SendHTML(ctx, "x"),
		"dépôt nil":           nilDepot.SendDocument(ctx, "f", []byte("x")),
		"dépôt sans salon":    NewDepot("123:abc", "").SendDocument(ctx, "f", []byte("x")),
	}
	for nom, err := range cas {
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("%s : %v, attendu ErrDisabled", nom, err)
		}
	}
	if nilBot.Enabled() || nilTG.Enabled() || nilDepot.Enabled() {
		t.Error("un notificateur nil doit se dire éteint")
	}
}

func TestEnabled(t *testing.T) {
	cas := []struct {
		token, chats string
		veut         bool
	}{
		{"", "", false},
		{"123:abc", "", false},
		{"", "-100123", false},
		{"123:abc", "-100123", true},
		{"123:abc", " , -100123 , -100456 ", true},
	}
	for _, c := range cas {
		if got := NewTelegram(c.token, c.chats).Enabled(); got != c.veut {
			t.Errorf("NewTelegram(%q, %q).Enabled() = %v, attendu %v", c.token, c.chats, got, c.veut)
		}
		if got := NewDepot(c.token, c.chats).Enabled(); got != c.veut {
			t.Errorf("NewDepot(%q, %q).Enabled() = %v, attendu %v", c.token, c.chats, got, c.veut)
		}
	}
}

// Plusieurs salons : chacun reçoit le message, un salon qui refuse n'arrête pas
// les autres, et l'erreur les nomme.
func TestTelegramEcritATousLesSalons(t *testing.T) {
	srv, recus := fauxTelegram(t, http.StatusBadRequest)
	tg := NewTelegram("123:abc", " -100123 ,, -100456 ")
	viser(tg.bot, srv.URL)

	err := tg.Send(context.Background(), "x")
	if err == nil {
		t.Fatal("deux refus doivent faire une erreur")
	}
	got := recus()
	if len(got) != 2 || got[0].chat != "-100123" || got[1].chat != "-100456" {
		t.Fatalf("reçu %+v, attendu un envoi à chacun des deux salons", got)
	}
	for _, chat := range []string{"-100123", "-100456"} {
		if !strings.Contains(err.Error(), chat) {
			t.Errorf("l'erreur ne nomme pas le salon %s : %v", chat, err)
		}
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("l'erreur doit porter l'explication de Telegram : %v", err)
	}
}

// Le jeton est dans le CHEMIN de la requête : une erreur de transport est une
// *url.Error qui porte l'URL entière, et elle part au journal.
func TestLeJetonNeFuitJamais(t *testing.T) {
	const jeton = "123456:AAHsupersecretbottoken"
	ctx := context.Background()

	b := NewBot(jeton)
	viser(b, "http://127.0.0.1:1") // un port fermé : l'échec a lieu dans le client
	d := NewDepot(jeton, "-100123")
	viser(d.bot, "http://127.0.0.1:1")

	for nom, err := range map[string]error{
		"Send":         b.Send(ctx, "1", "x"),
		"SendHTML":     b.SendHTML(ctx, "1", "x"),
		"SendDocument": d.SendDocument(ctx, "f.enc", []byte("x")),
	} {
		if err == nil {
			t.Fatalf("%s : un port fermé doit échouer", nom)
		}
		if strings.Contains(err.Error(), jeton) {
			t.Fatalf("%s : le jeton a fui dans l'erreur : %v", nom, err)
		}
		if !strings.Contains(err.Error(), "<token>") {
			t.Errorf("%s : le jeton devait être masqué, reçu : %v", nom, err)
		}
	}
}

func TestDepotPoseLeFichierDansChaqueSalon(t *testing.T) {
	srv, recus := fauxTelegram(t, http.StatusOK)
	d := NewDepot("123:abc", "-100777,-100888")
	viser(d.bot, srv.URL)

	if err := d.SendDocument(context.Background(), "todo-20260823T043500.000Z.db.gz.enc", []byte("octets scellés")); err != nil {
		t.Fatal(err)
	}
	got := recus()
	if len(got) != 2 {
		t.Fatalf("%d envois, attendu 2", len(got))
	}
	for i, chat := range []string{"-100777", "-100888"} {
		if got[i].path != "/bot123:abc/sendDocument" || got[i].chat != chat {
			t.Errorf("envoi %d : %+v", i, got[i])
		}
		if got[i].filename != "todo-20260823T043500.000Z.db.gz.enc" || string(got[i].data) != "octets scellés" {
			t.Errorf("envoi %d : fichier %q, %q", i, got[i].filename, got[i].data)
		}
	}
}

func TestDepotRefuseUnFichierTropGros(t *testing.T) {
	srv, recus := fauxTelegram(t, http.StatusOK)
	d := NewDepot("123:abc", "1")
	viser(d.bot, srv.URL)

	err := d.SendDocument(context.Background(), "gros.enc", make([]byte, docLimit+1))
	if err == nil || !strings.Contains(err.Error(), "gros.enc") {
		t.Fatalf("erreur = %v, attendu un refus qui nomme le fichier", err)
	}
	if n := len(recus()); n != 0 {
		t.Errorf("%d envois partis, attendu aucun", n)
	}
}

// docSender est la forme que Telegram et Bot ne doivent PAS avoir. gate et
// ateliers ne construisent que ceux-là : leur base (tous les comptes de la
// flotte, des notes cliniques) ne doit avoir aucun chemin vers un salon.
// Ajouter la méthode fait rougir ce test, et oblige à relire la note du paquet.
func TestSeulDepotEnvoieUnFichier(t *testing.T) {
	for _, v := range []any{NewTelegram("t", "1"), NewBot("t")} {
		ty := reflect.TypeOf(v)
		for i := 0; i < ty.NumMethod(); i++ {
			if m := ty.Method(i).Name; strings.Contains(strings.ToLower(m), "document") || strings.Contains(strings.ToLower(m), "file") {
				t.Errorf("%s a une méthode %s : seul Depot peut faire partir un fichier", ty, m)
			}
		}
	}
}

func TestTruncateCoupeEnFinDeLigne(t *testing.T) {
	long := strings.Repeat("une ligne de tâche\n", 400)
	got := truncate(long)
	if len(got) > maxText+len("\n…") {
		t.Fatalf("tronqué à %d octets, au-delà de %d", len(got), maxText)
	}
	if !strings.HasSuffix(got, "une ligne de tâche\n…") {
		t.Errorf("la coupe doit tomber en fin de ligne : …%q", got[len(got)-30:])
	}
	if court := "court"; truncate(court) != court {
		t.Errorf("un message court doit passer intact")
	}
}

func TestTruncateNeCoupePasUnCaractere(t *testing.T) {
	long := strings.Repeat("é", maxText) // deux octets chacun, aucune fin de ligne
	got := truncate(long)
	if !utf8.ValidString(got) {
		t.Fatal("la coupe a tranché un caractère en deux")
	}
	if len(got) > maxText+len("…") {
		t.Errorf("tronqué à %d octets", len(got))
	}
}
