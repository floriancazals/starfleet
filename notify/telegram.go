// Package notify porte les rares messages sortants de la flotte vers Telegram :
// une alerte de sauvegarde, une commande qui devient urgente, un rappel.
//
// Trois types, et le partage entre eux est la décision qui compte :
//
//   - Telegram écrit dans les salons de la configuration (TELEGRAM_CHAT_ID). Il ne
//     porte que du TEXTE : une app qui ne construit que lui n'a aucun chemin pour
//     faire partir un fichier, et aucune variable d'environnement ne peut lui en
//     donner un à deux heures du matin. C'est ce qui protège la base de gate (tous
//     les comptes de la flotte) et celle d'ateliers (des notes cliniques datées).
//   - Bot écrit à un salon choisi à chaque appel : todo écrit à chaque personne
//     dans sa propre conversation, ateliers à la praticienne. Texte aussi.
//   - Depot pose un fichier — une copie scellée de la base — dans ses salons. Une
//     app ne l'obtient qu'en le construisant elle-même, et seules celles dont les
//     données sont celles de leur auteur le font.
//
// Deux règles valent pour les trois. Un envoi sans configuration est une ERREUR
// (ErrDisabled), jamais un succès muet : lease et escale-breakfast marquent un
// rappel « envoyé » quand Send a réussi, et un rappel jamais parti n'est pas un
// rappel envoyé. Et le jeton du bot n'apparaît dans aucune erreur : il est dans le
// chemin de la requête, donc dans le message d'une *url.Error, qui part tout droit
// au journal.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// apiRoot est la racine de l'API des bots. C'est un champ de chaque Bot plutôt
// qu'une constante utilisée telle quelle, pour que les tests visent un serveur
// httptest ou un port fermé — le seul moyen d'exercer l'échec de transport, là
// où le jeton fuirait.
const apiRoot = "https://api.telegram.org"

// maxText borne un message en texte brut. Telegram refuse au-delà de 4096
// caractères ; la marge laisse la place à une ligne d'en-tête ajoutée en route.
const maxText = 3500

// docLimit est le plafond d'un envoi de fichier par un bot (50 Mo), vérifié ici
// pour qu'une copie trop grosse échoue avec cette phrase plutôt qu'avec un 413.
const docLimit = 50 << 20

// ErrDisabled dit qu'il manque le jeton du bot ou le salon : rien n'est parti.
var ErrDisabled = errors.New("telegram : aucun jeton de bot, ou aucun salon configuré")

// Bot parle à l'API Telegram au nom d'un bot. Il ne connaît aucun salon : chaque
// appel nomme le sien. Un Bot nil ou sans jeton est éteint.
type Bot struct {
	token   string
	api     string
	client  *http.Client
	uploads *http.Client // un fichier fait des mégaoctets sur une liaison montante de maison
}

// NewBot construit un bot à partir du jeton donné par @BotFather. Un jeton vide
// donne un bot éteint, ce qui est une façon prévue de faire tourner une app.
func NewBot(token string) *Bot {
	return &Bot{
		token:   strings.TrimSpace(token),
		api:     apiRoot,
		client:  &http.Client{Timeout: 10 * time.Second},
		uploads: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Enabled dit si le bot a un jeton.
func (b *Bot) Enabled() bool { return b != nil && b.token != "" }

// Send envoie un texte brut à un salon. Un texte trop long est coupé à la
// dernière fin de ligne qui tient, et le dit par « … ».
func (b *Bot) Send(ctx context.Context, chatID, text string) error {
	return b.message(ctx, chatID, truncate(text), "")
}

// SendHTML envoie un message mis en forme en HTML (parse_mode HTML) : c'est à
// l'appelant d'échapper ce qu'il y met. Il n'est pas tronqué — couper du HTML
// laisserait une balise ouverte, que Telegram refuse.
func (b *Bot) SendHTML(ctx context.Context, chatID, html string) error {
	return b.message(ctx, chatID, html, "HTML")
}

func (b *Bot) message(ctx context.Context, chatID, text, parseMode string) error {
	if !b.Enabled() || chatID == "" {
		return ErrDisabled
	}
	form := url.Values{
		"chat_id":                  {chatID},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	if parseMode != "" {
		form.Set("parse_mode", parseMode)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("sendMessage"), strings.NewReader(form.Encode()))
	if err != nil {
		return b.fail("sendMessage", chatID, err.Error())
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(b.client, req, "sendMessage", chatID)
}

// sendDocument pose un fichier dans un salon. Il n'est pas exporté : seul Depot
// s'en sert, pour que l'envoi d'un fichier reste un type qu'on construit exprès.
func (b *Bot) sendDocument(ctx context.Context, chatID, filename string, data []byte) error {
	if !b.Enabled() || chatID == "" {
		return ErrDisabled
	}
	if len(data) > docLimit {
		return fmt.Errorf("telegram sendDocument : %s fait %d octets, au-delà des %d qu'un bot peut envoyer",
			filename, len(data), docLimit)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", chatID); err != nil {
		return err
	}
	part, err := mw.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint("sendDocument"), &body)
	if err != nil {
		return b.fail("sendDocument", chatID, err.Error())
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return b.do(b.uploads, req, "sendDocument", chatID)
}

func (b *Bot) endpoint(method string) string {
	return strings.TrimRight(b.api, "/") + "/bot" + b.token + "/" + method
}

// do envoie la requête et change tout ce qui n'est pas un 2xx en erreur sans jeton.
func (b *Bot) do(client *http.Client, req *http.Request, method, chatID string) error {
	resp, err := client.Do(req)
	if err != nil {
		return b.fail(method, chatID, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Telegram explique ses refus dans le corps ; sans lui, le journal ne dirait que « 400 ».
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return b.fail(method, chatID, resp.Status+" : "+strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

// fail fabrique l'erreur d'un envoi, jeton effacé. L'erreur d'origine est aplatie
// en texte plutôt qu'enveloppée : rien ne doit pouvoir ressortir l'URL entière.
func (b *Bot) fail(method, chatID, msg string) error {
	if b.token != "" {
		msg = strings.ReplaceAll(msg, b.token, "<token>")
	}
	return fmt.Errorf("telegram %s salon %s : %s", method, chatID, msg)
}

// truncate coupe un texte brut trop long à la dernière fin de ligne qui tient,
// sans jamais couper un caractère en deux.
func truncate(msg string) string {
	if len(msg) <= maxText {
		return msg
	}
	cut := maxText
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(msg[:cut], '\n'); i > 0 {
		return msg[:i] + "\n…"
	}
	return msg[:cut] + "…"
}

// Telegram écrit dans les salons de la configuration. Il ne porte que du texte :
// voir la note du paquet.
type Telegram struct {
	bot   *Bot
	chats []string
}

// NewTelegram construit un notificateur. chatIDs est une liste séparée par des
// virgules ; un jeton ou une liste vide donnent un notificateur éteint.
func NewTelegram(token, chatIDs string) *Telegram {
	return &Telegram{bot: NewBot(token), chats: splitChats(chatIDs)}
}

// Enabled dit si les messages partiront quelque part.
func (t *Telegram) Enabled() bool { return t != nil && t.bot.Enabled() && len(t.chats) > 0 }

// Send envoie un texte brut à chaque salon.
func (t *Telegram) Send(ctx context.Context, text string) error {
	return t.each(func(chat string) error { return t.bot.Send(ctx, chat, text) })
}

// SendHTML envoie un message HTML à chaque salon.
func (t *Telegram) SendHTML(ctx context.Context, html string) error {
	return t.each(func(chat string) error { return t.bot.SendHTML(ctx, chat, html) })
}

// each essaie tous les salons même après un échec — un salon injoignable ne doit
// pas faire taire les autres — et rend toutes les erreurs ensemble.
func (t *Telegram) each(send func(chat string) error) error {
	if !t.Enabled() {
		return ErrDisabled
	}
	return fanOut(t.chats, send)
}

// Depot pose des fichiers dans ses salons : c'est par lui que la copie scellée
// d'une base quitte la machine, pour les apps dont les données sont celles de leur
// auteur. Ce qu'on lui confie doit être chiffré d'avance — Telegram est un
// serveur de discussion, pas un coffre.
type Depot struct {
	bot   *Bot
	chats []string
}

// NewDepot construit un dépôt. chatIDs est une liste séparée par des virgules.
func NewDepot(token, chatIDs string) *Depot {
	return &Depot{bot: NewBot(token), chats: splitChats(chatIDs)}
}

// Enabled dit si un fichier partira quelque part.
func (d *Depot) Enabled() bool { return d != nil && d.bot.Enabled() && len(d.chats) > 0 }

// SendDocument pose le fichier dans chaque salon du dépôt.
func (d *Depot) SendDocument(ctx context.Context, filename string, data []byte) error {
	if !d.Enabled() {
		return ErrDisabled
	}
	if len(data) > docLimit {
		return fmt.Errorf("telegram sendDocument : %s fait %d octets, au-delà des %d qu'un bot peut envoyer",
			filename, len(data), docLimit)
	}
	return fanOut(d.chats, func(chat string) error { return d.bot.sendDocument(ctx, chat, filename, data) })
}

func fanOut(chats []string, send func(chat string) error) error {
	var errs []error
	for _, chat := range chats {
		if err := send(chat); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func splitChats(chatIDs string) []string {
	var chats []string
	for _, c := range strings.Split(chatIDs, ",") {
		if c = strings.TrimSpace(c); c != "" {
			chats = append(chats, c)
		}
	}
	return chats
}
