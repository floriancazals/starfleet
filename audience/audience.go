// Package audience mesure l'usage de l'app et l'envoie à l'Umami de la flotte
// (umami.fcazals.fr, sur le VPS) : quelles pages sont ouvertes, quelles actions
// sont faites, combien de visiteurs, sur quel téléphone ou quel ordinateur.
//
// C'est le SERVEUR qui envoie, pas le navigateur — et c'est le choix qui porte
// tout le reste. Le script d'Umami aurait demandé trois choses que la flotte
// refuse : ouvrir la CSP de chaque app à une origine tierce (donc laisser cette
// origine exécuter du code dans gate ou dans ateliers) ; envoyer l'adresse
// RÉELLE de chaque page, alors que des adresses de la flotte portent des
// secrets (lien de dossier folio, lien d'enrôlement gate, lien de connexion) ;
// et se laisser éteindre par n'importe quel bloqueur de publicité.
//
// Ce qui part, et rien d'autre :
//
//   - une PAGE VUE par navigation réussie : un GET de page servi en 2xx, ou
//     redirigé vers un AUTRE site (le lien d'invitation qui ouvre Home
//     Assistant, la connexion qui part chez gate) ;
//   - un ÉVÉNEMENT par action acceptée : un POST (ou PUT, PATCH, DELETE) envoyé
//     par une page de l'app elle-même, servi sous 400 — une redirection après
//     un refus (« ?err=… ») compte donc aussi : c'est une tentative, donc un
//     usage —, et nommé par sa route : « POST /semaine/affectation ».
//
// L'adresse envoyée est le MOTIF de la route (« /equipe/{id} »), jamais le
// chemin demandé : aucun identifiant, aucun jeton, aucune requête — seuls les
// paramètres utm_* passent, faits pour ça. La provenance se réduit à l'origine
// d'un site TIERS ; celle d'une page de l'app ne part pas.
//
// L'adresse IP et le navigateur du visiteur partent aussi, parce qu'Umami en a
// besoin pour compter des visiteurs : il en tire un pays et un identifiant de
// visite haché avec un sel qui change chaque mois, et ne garde pas l'adresse.
//
// Rien ne ralentit ni ne casse une réponse : la mesure est notée APRÈS la
// réponse, dans une file bornée qu'un seul goroutine vide. File pleine ou Umami
// muet, la mesure est perdue et la page est servie. Et une configuration vide
// éteint tout : l'app est alors exactement celle d'avant.
//
// Toutes les apps de la flotte importent ce paquet (chacune en gardait une
// copie jusqu'en octobre 2026) : une correction ici vaut pour toutes, au
// prochain relèvement de version de starfleet dans chaque app.
package audience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Taille de la file d'attente. Une app de la flotte sert quelques pages par
// minute : 256 mesures en attente veulent dire qu'Umami ne répond plus depuis
// longtemps, et il vaut mieux alors perdre les suivantes que de les garder en
// mémoire.
const capaciteFile = 256

// delaiEnvoi borne un envoi. Umami est sur une autre machine, joint par
// Internet ; un envoi plus long que ça n'arrivera pas.
const delaiEnvoi = 5 * time.Second

// Audience envoie la mesure d'une app. Une Audience nil est ÉTEINTE, et toutes
// ses méthodes l'acceptent : l'appelant n'a rien à tester avant de s'en servir.
type Audience struct {
	point  string // https://umami…/api/send
	site   string
	log    *slog.Logger
	client *http.Client
	file   chan envoi

	mu      sync.Mutex
	enPanne bool // un échec a été dit, le suivant ne le sera pas
	perdues int  // mesures perdues depuis
}

// envoi est le corps attendu par POST /api/send d'Umami (v3).
type envoi struct {
	Type    string `json:"type"`
	Payload charge `json:"payload"`
}

type charge struct {
	Website   string `json:"website"`
	Hostname  string `json:"hostname,omitempty"`
	URL       string `json:"url"`
	Referrer  string `json:"referrer,omitempty"`
	Language  string `json:"language,omitempty"`
	Name      string `json:"name,omitempty"`
	IP        string `json:"ip,omitempty"`
	UserAgent string `json:"userAgent"`
	// Pas de « timestamp » : Umami ne le prend qu'en SECONDES entières, et deux
	// pages ouvertes dans la même seconde s'y retrouvaient dans le désordre
	// (mesuré). Sans lui, Umami date à la réception, à la milliseconde — la
	// file ne retient une mesure que quelques millisecondes.
}

// Nouveau lit la configuration : l'adresse d'Umami (UMAMI_URL) et l'identifiant
// du site de CETTE instance de l'app (UMAMI_SITE).
//
// Les deux vides, la mesure est éteinte : c'est un état normal, et Nouveau rend
// nil sans erreur. L'une sans l'autre, ou illisible, c'est une erreur — rendue
// pour que le lancement la DISE. Elle n'empêche pas l'app de se lancer : on ne
// coupe pas un service pour une statistique.
func Nouveau(adresse, site string, log *slog.Logger) (*Audience, error) {
	adresse = strings.TrimSpace(adresse)
	site = strings.TrimSpace(site)
	switch {
	case adresse == "" && site == "":
		return nil, nil
	case adresse == "":
		return nil, errors.New("UMAMI_SITE est posé mais UMAMI_URL est vide")
	case site == "":
		return nil, errors.New("UMAMI_URL est posé mais UMAMI_SITE est vide")
	}
	u, err := url.Parse(adresse)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("UMAMI_URL illisible (attendu https://hôte) : %q", adresse)
	}
	if !estUnUUID(site) {
		return nil, fmt.Errorf("UMAMI_SITE n'est pas un identifiant de site Umami (un UUID) : %q", site)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Audience{
		point:  strings.TrimRight(u.String(), "/") + "/api/send",
		site:   strings.ToLower(site),
		log:    log,
		client: &http.Client{Timeout: delaiEnvoi},
		file:   make(chan envoi, capaciteFile),
	}, nil
}

// Demarre est le câblage d'une app en une ligne : lire la configuration, DIRE
// l'état de la mesure dans le journal de lancement, et lancer l'envoi pour la
// vie du processus. Elle rend toujours une valeur utilisable — éteinte, sa
// méthode Mesure rend le serveur tel quel.
//
// Pas de contexte d'arrêt à passer, et c'est voulu : dans la moitié des apps de
// la flotte il naît APRÈS le serveur HTTP, et ce qui reste en file à l'arrêt
// est perdu de toute façon.
func Demarre(adresse, site string, log *slog.Logger) *Audience {
	if log == nil {
		log = slog.Default()
	}
	a, err := Nouveau(adresse, site, log)
	switch {
	case err != nil:
		log.Error("mesure d'audience ÉTEINTE : configuration illisible", "error", err)
	case a == nil:
		log.Info("mesure d'audience éteinte (UMAMI_URL et UMAMI_SITE vides)")
	default:
		log.Info("mesure d'audience active", "umami", a.point, "site", a.site)
		go a.Tourne(context.Background())
	}
	return a
}

// Active dit si la mesure part.
func (a *Audience) Active() bool { return a != nil }

// Tourne vide la file jusqu'à la fin du contexte. Ce qui reste en file à
// l'arrêt est perdu : c'est une mesure, pas une écriture.
func (a *Audience) Tourne(ctx context.Context) {
	if a == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-a.file:
			a.envoie(ctx, e)
		}
	}
}

// Mesure compte ce que sert h — en pratique le serveur entier de l'app.
//
// Le motif de la route est lu APRÈS la réponse, dans r.Pattern : le routeur de
// la bibliothèque standard le pose sur la requête qu'il reçoit, et toutes les
// couches de la flotte entre ici et lui (en-têtes de sécurité, barre oblique
// finale) transmettent la MÊME requête. Une couche qui en ferait une copie
// (r.WithContext) AVANT le routeur rendrait la mesure muette, sans
// erreur : chaque app a un test qui fait passer une vraie navigation par son
// vrai serveur pour le vérifier.
func (a *Audience) Mesure(h http.Handler) http.Handler {
	if a == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := sorteDe(r)
		if s == rien {
			h.ServeHTTP(w, r)
			return
		}
		t := &temoin{ResponseWriter: w}
		h.ServeHTTP(t, r)
		a.note(s, r, t.statut(), w.Header().Get("Location"))
	})
}

type sorte int

const (
	rien sorte = iota
	pageVue
	action
)

// sorteDe trie les requêtes sur les en-têtes Sec-Fetch-*, que seul un
// navigateur envoie (Chrome, Firefox, et Safari depuis 16.4). Une requête qui
// n'en porte pas — curl, un robot, l'API robot de todo, la sonde de santé,
// la poussée de révocation de gate — n'est jamais comptée.
func sorteDe(r *http.Request) sorte {
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "" || r.UserAgent() == "" {
		return rien
	}
	switch r.Method {
	case http.MethodGet:
		// Une navigation vers une page. Les fragments htmx, les fetch, les
		// images, les feuilles de style n'en sont pas ; un préchargement non
		// plus — personne n'a encore vu la page.
		//
		// Le piège, mesuré dans un vrai Chromium : une navigation relayée par le
		// SERVICE WORKER d'une app installable arrive avec Sec-Fetch-Mode
		// « navigate » mais Sec-Fetch-Dest « empty » — c'est le worker qui
		// refait la requête, et un fetch n'a pas de destination. N'exiger que
		// « document » rendait la mesure aveugle à toutes les pages d'une app
		// une fois son worker installé, sans un mot nulle part. Une page ne
		// peut pas fabriquer une requête « navigate » avec fetch : ce mode
		// n'appartient qu'au navigateur et à ce relais.
		if r.Header.Get("Sec-Fetch-Mode") != "navigate" {
			return rien
		}
		if d := r.Header.Get("Sec-Fetch-Dest"); d != "document" && d != "empty" {
			return rien // un cadre, un objet : pas une page qu'on ouvre
		}
		if prechargement(r) {
			return rien
		}
		return pageVue
	case http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodConnect:
		return rien
	default:
		// Une action ne compte que lancée par une page de l'app : un formulaire
		// posté depuis ailleurs n'est pas un usage, c'est au mieux une erreur.
		if site != "same-origin" {
			return rien
		}
		return action
	}
}

func prechargement(r *http.Request) bool {
	for _, en := range []string{"Sec-Purpose", "Purpose", "X-Purpose", "X-Moz"} {
		v := strings.ToLower(r.Header.Get(en))
		if strings.Contains(v, "prefetch") || strings.Contains(v, "prerender") || strings.Contains(v, "preview") {
			return true
		}
	}
	return false
}

// note fabrique la mesure et la pose dans la file, sans jamais attendre.
func (a *Audience) note(s sorte, r *http.Request, statut int, destination string) {
	switch s {
	case pageVue:
		// Une redirection n'est pas une page vue : la page d'arrivée sera
		// comptée elle-même. Sauf quand elle mène AILLEURS — escale-invite
		// n'a pas d'autre usage qu'un 303 vers Home Assistant, et rien d'autre
		// ne le compterait. L'adresse de destination, elle, ne part pas : elle
		// porte le code de connexion.
		redirigeAilleurs := statut >= 300 && statut < 400 && ailleurs(r, destination)
		if (statut < 200 || statut >= 300) && !redirigeAilleurs {
			return
		}
	case action:
		// 303 après un enregistrement réussi est la règle de la flotte ; 422,
		// c'est un formulaire refusé, et 403 une action interdite.
		if statut < 200 || statut >= 400 {
			return
		}
	default:
		return
	}
	chemin := cheminDuMotif(r.Pattern)
	if chemin == "" {
		return // aucune route : rien à nommer
	}
	c := charge{
		Website:   a.site,
		Hostname:  hote(r),
		URL:       chemin + utm(r.URL.Query()),
		Referrer:  provenance(r),
		Language:  langue(r.Header.Get("Accept-Language")),
		IP:        adresseVisiteur(r),
		UserAgent: r.UserAgent(),
	}
	if s == action {
		c.Name = r.Method + " " + chemin
	}
	select {
	case a.file <- envoi{Type: "event", Payload: c}:
	default:
		a.echec(errors.New("file d'attente pleine, Umami ne suit plus"))
	}
}

// cheminDuMotif rend la partie chemin d'un motif du routeur :
//
//	« GET /equipe/{id} »        → « /equipe/{id} »
//	« POST /admin/annonce/{$} » → « /admin/annonce/ »
//	« GET /{$} »                → « / »
//	« hote.fr/x »               → « /x »
//
// et "" quand aucune route n'a répondu.
func cheminDuMotif(motif string) string {
	champs := strings.Fields(motif)
	if len(champs) == 0 {
		return ""
	}
	p := champs[len(champs)-1]
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(p[i:], "{$}")
}

// utm garde les paramètres de campagne, et eux seuls : ils sont faits pour être
// lus (une affiche, une publication), alors que le reste d'une requête peut
// porter un jeton.
func utm(q url.Values) string {
	garde := url.Values{}
	for _, k := range []string{"utm_source", "utm_medium", "utm_campaign", "utm_content", "utm_term"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			garde.Set(k, tronque(v, 100))
		}
	}
	if len(garde) == 0 {
		return ""
	}
	return "?" + garde.Encode()
}

// provenance rend l'origine d'un site TIERS d'où vient le visiteur, et rien
// quand il vient de l'app elle-même : l'adresse de la page précédente est une
// adresse de l'app, avec ses identifiants et ses jetons.
func provenance(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	if strings.EqualFold(u.Hostname(), hote(r)) {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + "/"
}

// ailleurs dit si une redirection quitte l'app : une adresse complète vers un
// autre hôte. « /semaine » ou « ?err=… » restent ici.
func ailleurs(r *http.Request, destination string) bool {
	u, err := url.Parse(destination)
	if err != nil || u.Host == "" {
		return false
	}
	return !strings.EqualFold(u.Hostname(), hote(r))
}

func hote(r *http.Request) string {
	h := r.Host
	if sansPort, _, err := net.SplitHostPort(h); err == nil {
		h = sansPort
	}
	return strings.ToLower(h)
}

// langue rend la première langue d'Accept-Language (« fr-FR »), comme le
// ferait navigator.language.
func langue(accepte string) string {
	l, _, _ := strings.Cut(accepte, ",")
	l, _, _ = strings.Cut(l, ";")
	l = strings.TrimSpace(l)
	if l == "*" {
		return ""
	}
	return tronque(l, 35)
}

// adresseVisiteur lit X-Real-Ip, la seule lecture juste partout dans la flotte :
// derrière les deux Traefik du Pi 5, X-Forwarded-For porte deux entrées et
// aucune règle simple n'y trouve le visiteur. Sans en-tête, c'est l'adresse de
// la connexion (un banc local).
func adresseVisiteur(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); net.ParseIP(ip) != nil {
		return ip
	}
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && net.ParseIP(ip) != nil {
		return ip
	}
	return ""
}

func (a *Audience) envoie(ctx context.Context, e envoi) {
	corps, err := json.Marshal(e)
	if err != nil {
		a.echec(err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.point, bytes.NewReader(corps))
	if err != nil {
		a.echec(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	rep, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // arrêt du service, pas une panne d'Umami
		}
		a.echec(err)
		return
	}
	defer func() { _ = rep.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(rep.Body, 64<<10))
	if rep.StatusCode != http.StatusOK {
		// 400 « Website not found » : UMAMI_SITE ne désigne aucun site.
		a.echec(fmt.Errorf("réponse d'Umami : %s", rep.Status))
		return
	}
	a.reussite()
}

// echec dit une panne UNE fois, puis se tait jusqu'au retour : Umami éteint ne
// doit pas remplir le journal de chaque app d'une ligne par page vue.
func (a *Audience) echec(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.perdues++
	if !a.enPanne {
		a.enPanne = true
		a.log.Warn("mesure d'audience perdue — les suivantes le seront sans autre message jusqu'au retour d'Umami",
			"umami", a.point, "error", err)
	}
}

// reussite dit le retour, avec ce qu'a coûté la panne.
func (a *Audience) reussite() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enPanne {
		a.log.Info("mesure d'audience rétablie", "perdues", a.perdues)
		a.enPanne = false
		a.perdues = 0
	}
}

// temoin retient le statut de la réponse, et ne change rien d'autre.
type temoin struct {
	http.ResponseWriter
	code int
}

func (t *temoin) WriteHeader(code int) {
	if t.code == 0 && code >= 200 {
		t.code = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *temoin) Write(b []byte) (int, error) {
	if t.code == 0 {
		t.code = http.StatusOK
	}
	return t.ResponseWriter.Write(b)
}

// Flush garde le temoin transparent pour un handler qui pousserait sa réponse
// par morceaux. Pousser fige l'en-tête : sans statut écrit, c'est un 200.
func (t *temoin) Flush() {
	if t.code == 0 {
		t.code = http.StatusOK
	}
	_ = http.NewResponseController(t.ResponseWriter).Flush()
}

// Unwrap rend l'écrivain d'origine à http.ResponseController : les apps qui
// repoussent l'échéance de lecture d'un envoi de photos la retrouvent à
// travers le temoin.
func (t *temoin) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (t *temoin) statut() int {
	if t.code == 0 {
		return http.StatusOK // un handler qui n'écrit rien répond 200
	}
	return t.code
}

func estUnUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

func tronque(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
