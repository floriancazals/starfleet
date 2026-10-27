package audience

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const siteTest = "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"

// Ce fichier épingle surtout ce qui NE PART PAS. Une mesure en trop ne se voit
// nulle part : ni dans l'app, ni dans son journal — seulement, un jour, dans la
// base d'Umami, où un jeton de lien ou un identifiant de dossier n'a rien à
// faire.

func routeur() *http.ServeMux {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	vers := func(cible string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, cible, http.StatusSeeOther) }
	}
	mux.HandleFunc("GET /{$}", ok)
	mux.HandleFunc("GET /semaine", ok)
	mux.HandleFunc("GET /equipe/{id}", ok)
	mux.HandleFunc("GET /redirige", vers("/semaine"))
	mux.HandleFunc("GET /redirige-ici", vers("https://releve.fcazals.fr/semaine"))
	mux.HandleFunc("GET /i/{jeton}", vers("https://ha.exemple.fr/lovelace?code=c0de-de-connexion"))
	mux.HandleFunc("GET /panne", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panne", http.StatusInternalServerError)
	})
	mux.HandleFunc("POST /semaine/affectation", vers("/semaine"))
	mux.HandleFunc("POST /comptes/{sub}/supprimer", vers("/comptes"))
	mux.HandleFunc("POST /admin/annonce/{$}", vers("/admin/"))
	mux.HandleFunc("POST /refuse", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	return mux
}

// navigation fabrique ce qu'envoie un navigateur qui ouvre une page.
func navigation(cible string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, cible, nil)
	r.Header.Set("Sec-Fetch-Site", "none")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15")
	r.Header.Set("X-Real-Ip", "203.0.113.7")
	r.Header.Set("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.8")
	return r
}

// formulaire fabrique ce qu'envoie un formulaire posté depuis une page de l'app.
func formulaire(cible string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, cible, strings.NewReader("a=b"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0")
	r.Header.Set("X-Real-Ip", "203.0.113.8")
	return r
}

// allumee rend une Audience dont personne ne vide la file : le test y lit
// directement ce qui serait parti.
func allumee(t *testing.T) *Audience {
	t.Helper()
	a, err := Nouveau("https://umami.exemple.fr", siteTest, slog.New(slog.DiscardHandler))
	if err != nil || a == nil {
		t.Fatalf("Nouveau : %v", err)
	}
	return a
}

func sert(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// seule rend LA mesure en file, et échoue s'il n'y en a pas exactement une.
func seule(t *testing.T, a *Audience) charge {
	t.Helper()
	if n := len(a.file); n != 1 {
		t.Fatalf("%d mesure(s) en file, attendu 1", n)
	}
	e := <-a.file
	if e.Type != "event" {
		t.Errorf("type = %q, attendu event", e.Type)
	}
	return e.Payload
}

func TestEteinteLeServeurEstRenduTelQuel(t *testing.T) {
	a, err := Nouveau("", "", nil)
	if err != nil || a != nil {
		t.Fatalf("Nouveau(vide) = %v, %v ; attendu nil, nil", a, err)
	}
	mux := routeur()
	if h := a.Mesure(mux); h != http.Handler(mux) {
		t.Error("éteinte, Mesure doit rendre le serveur lui-même, sans rien autour")
	}
	if a.Active() {
		t.Error("Active() = true pour une audience éteinte")
	}
	a.Tourne(context.Background()) // ne doit pas bloquer
}

func TestUneConfigurationIllisibleEstUneErreur(t *testing.T) {
	for _, c := range []struct{ nom, adresse, site string }{
		{"site sans adresse", "", siteTest},
		{"adresse sans site", "https://umami.fcazals.fr", ""},
		{"adresse sans schéma", "umami.fcazals.fr", siteTest},
		{"schéma inconnu", "ftp://umami.fcazals.fr", siteTest},
		{"site qui n'est pas un UUID", "https://umami.fcazals.fr", "releve"},
		{"UUID tronqué", "https://umami.fcazals.fr", siteTest[:35]},
	} {
		t.Run(c.nom, func(t *testing.T) {
			if a, err := Nouveau(c.adresse, c.site, nil); err == nil || a != nil {
				t.Errorf("Nouveau(%q, %q) = %v, %v ; attendu une erreur", c.adresse, c.site, a, err)
			}
		})
	}
}

func TestUnePageVueEnvoieLeMotifJamaisLeChemin(t *testing.T) {
	a := allumee(t)
	r := navigation("https://releve.fcazals.fr/equipe/4217?jeton=s3cr3t&utm_source=affiche&utm_medium=papier")
	r.Header.Set("Referer", "https://www.google.com/search?q=planning+interne")
	if rec := sert(a.Mesure(routeur()), r); rec.Code != http.StatusOK {
		t.Fatalf("code = %d, la page doit être servie", rec.Code)
	}

	c := seule(t, a)
	attendu := charge{
		Website:   siteTest,
		Hostname:  "releve.fcazals.fr",
		URL:       "/equipe/{id}?utm_medium=papier&utm_source=affiche",
		Referrer:  "https://www.google.com/",
		Language:  "fr-FR",
		IP:        "203.0.113.7",
		UserAgent: r.UserAgent(),
	}
	if c != attendu {
		t.Errorf("mesure =\n  %+v\nattendu\n  %+v", c, attendu)
	}

	brut, _ := json.Marshal(c)
	for _, secret := range []string{"4217", "s3cr3t", "jeton", "search", "interne"} {
		if bytes.Contains(brut, []byte(secret)) {
			t.Errorf("%q est parti chez Umami : %s", secret, brut)
		}
	}
}

func TestLaPagePrecedenteDeLAppNeSortPas(t *testing.T) {
	a := allumee(t)
	r := navigation("https://releve.fcazals.fr/semaine")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Referer", "https://releve.fcazals.fr/equipe/4217?jeton=s3cr3t")
	sert(a.Mesure(routeur()), r)
	if c := seule(t, a); c.Referrer != "" {
		t.Errorf("provenance = %q, attendu rien : c'est une adresse de l'app", c.Referrer)
	}
}

func TestLaRacineSAppelleBarreOblique(t *testing.T) {
	a := allumee(t)
	sert(a.Mesure(routeur()), navigation("https://releve.fcazals.fr/"))
	if c := seule(t, a); c.URL != "/" {
		t.Errorf("url = %q, attendu « / » (le motif est « GET /{$} »)", c.URL)
	}
}

// Mesuré dans un vrai Chromium : une fois le service worker d'une app
// installable en place, c'est lui qui refait chaque navigation, et la requête
// arrive avec Sec-Fetch-Dest « empty » au lieu de « document ».
func TestUneNavigationRelayeeParLeServiceWorkerEstUnePageVue(t *testing.T) {
	a := allumee(t)
	r := navigation("https://releve.fcazals.fr/semaine")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Dest", "empty")
	sert(a.Mesure(routeur()), r)
	if c := seule(t, a); c.URL != "/semaine" {
		t.Errorf("url = %q, attendu /semaine", c.URL)
	}
}

// escale-invite n'a pas d'autre usage qu'une redirection vers Home Assistant :
// une navigation qui mène AILLEURS est une page vue — sans sa destination, qui
// porte un code de connexion.
func TestUneRedirectionVersUnAutreSiteEstUnePageVue(t *testing.T) {
	a := allumee(t)
	rec := sert(a.Mesure(routeur()), navigation("https://invite.fcazals.fr/i/s3cr3t-du-sejour"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, attendu 303", rec.Code)
	}
	c := seule(t, a)
	if c.URL != "/i/{jeton}" {
		t.Errorf("url = %q, attendu le motif /i/{jeton}", c.URL)
	}
	brut, _ := json.Marshal(c)
	for _, secret := range []string{"s3cr3t", "c0de", "ha.exemple.fr"} {
		if bytes.Contains(brut, []byte(secret)) {
			t.Errorf("%q est parti chez Umami : %s", secret, brut)
		}
	}
}

func TestUneActionEstNommeeParSaRoute(t *testing.T) {
	for _, c := range []struct{ cible, nom, url string }{
		{"https://releve.fcazals.fr/semaine/affectation", "POST /semaine/affectation", "/semaine/affectation"},
		{"https://releve.fcazals.fr/comptes/b6a1c0de/supprimer", "POST /comptes/{sub}/supprimer", "/comptes/{sub}/supprimer"},
		{"https://releve.fcazals.fr/admin/annonce/", "POST /admin/annonce/", "/admin/annonce/"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			a := allumee(t)
			if rec := sert(a.Mesure(routeur()), formulaire(c.cible)); rec.Code != http.StatusSeeOther {
				t.Fatalf("code = %d, attendu 303", rec.Code)
			}
			m := seule(t, a)
			if m.Name != c.nom || m.URL != c.url {
				t.Errorf("événement = %q sur %q, attendu %q sur %q", m.Name, m.URL, c.nom, c.url)
			}
			if m.IP != "203.0.113.8" {
				t.Errorf("ip = %q", m.IP)
			}
		})
	}
}

func TestCeQuiNeCompteJamais(t *testing.T) {
	type cas struct {
		nom string
		req func() *http.Request
	}
	avec := func(r *http.Request, en, val string) *http.Request {
		if val == "" {
			r.Header.Del(en)
		} else {
			r.Header.Set(en, val)
		}
		return r
	}
	page := "https://releve.fcazals.fr/semaine"
	for _, c := range []cas{
		{"fragment htmx", func() *http.Request {
			r := avec(navigation(page), "Sec-Fetch-Mode", "cors")
			r.Header.Set("HX-Request", "true")
			return avec(r, "Sec-Fetch-Dest", "empty")
		}},
		{"image", func() *http.Request {
			return avec(avec(navigation(page), "Sec-Fetch-Mode", "no-cors"), "Sec-Fetch-Dest", "image")
		}},
		{"page dans un cadre", func() *http.Request { return avec(navigation(page), "Sec-Fetch-Dest", "iframe") }},
		{"préchargement", func() *http.Request { return avec(navigation(page), "Sec-Purpose", "prefetch") }},
		{"prérendu", func() *http.Request { return avec(navigation(page), "Sec-Purpose", "prefetch;prerender") }},
		{"préchargement d'avant Sec-Purpose", func() *http.Request { return avec(navigation(page), "Purpose", "prefetch") }},
		{"curl ou un robot (aucun Sec-Fetch)", func() *http.Request {
			return avec(avec(avec(navigation(page), "Sec-Fetch-Site", ""), "Sec-Fetch-Mode", ""), "Sec-Fetch-Dest", "")
		}},
		{"sans navigateur nommé", func() *http.Request { return avec(navigation(page), "User-Agent", "") }},
		{"HEAD", func() *http.Request {
			r := navigation(page)
			r.Method = http.MethodHead
			return r
		}},
		{"redirection", func() *http.Request { return navigation("https://releve.fcazals.fr/redirige") }},
		{"redirection complète vers l'app elle-même", func() *http.Request {
			return navigation("https://releve.fcazals.fr/redirige-ici")
		}},
		{"aucune route", func() *http.Request { return navigation("https://releve.fcazals.fr/n-existe-pas") }},
		{"page en panne", func() *http.Request { return navigation("https://releve.fcazals.fr/panne") }},
		{"formulaire refusé (422)", func() *http.Request { return formulaire("https://releve.fcazals.fr/refuse") }},
		{"formulaire posté depuis un autre site", func() *http.Request {
			return avec(formulaire("https://releve.fcazals.fr/semaine/affectation"), "Sec-Fetch-Site", "cross-site")
		}},
		{"formulaire posté depuis une autre app de la flotte", func() *http.Request {
			return avec(formulaire("https://releve.fcazals.fr/semaine/affectation"), "Sec-Fetch-Site", "same-site")
		}},
		{"POST sans Sec-Fetch (l'API robot, la révocation de gate)", func() *http.Request {
			return avec(formulaire("https://releve.fcazals.fr/semaine/affectation"), "Sec-Fetch-Site", "")
		}},
	} {
		t.Run(c.nom, func(t *testing.T) {
			a := allumee(t)
			sert(a.Mesure(routeur()), c.req())
			if n := len(a.file); n != 0 {
				t.Errorf("%d mesure(s) en file, attendu aucune : %+v", n, (<-a.file).Payload)
			}
		})
	}
}

func TestAdresseDuVisiteur(t *testing.T) {
	r := navigation("https://releve.fcazals.fr/semaine")
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 192.168.1.3")
	if ip := adresseVisiteur(r); ip != "203.0.113.7" {
		t.Errorf("ip = %q, attendu X-Real-Ip et jamais X-Forwarded-For", ip)
	}
	r.Header.Set("X-Real-Ip", "pas une adresse")
	r.RemoteAddr = "192.0.2.10:51234"
	if ip := adresseVisiteur(r); ip != "192.0.2.10" {
		t.Errorf("ip = %q, attendu l'adresse de la connexion quand X-Real-Ip est illisible", ip)
	}
}

func TestLangue(t *testing.T) {
	for entree, attendu := range map[string]string{
		"fr-FR,fr;q=0.9,en;q=0.8": "fr-FR",
		"en;q=0.8":                "en",
		"*":                       "",
		"":                        "",
	} {
		if l := langue(entree); l != attendu {
			t.Errorf("langue(%q) = %q, attendu %q", entree, l, attendu)
		}
	}
}

func TestCheminDuMotif(t *testing.T) {
	for motif, attendu := range map[string]string{
		"GET /equipe/{id}":        "/equipe/{id}",
		"POST /admin/annonce/{$}": "/admin/annonce/",
		"GET /{$}":                "/",
		"/static/":                "/static/",
		"GET releve.fr/semaine":   "/semaine",
		"GET /fichiers/{rest...}": "/fichiers/{rest...}",
		"":                        "",
	} {
		if c := cheminDuMotif(motif); c != attendu {
			t.Errorf("cheminDuMotif(%q) = %q, attendu %q", motif, c, attendu)
		}
	}
}

// fauxUmami tient lieu d'Umami : il garde ce qu'on lui envoie et répond le
// statut qu'on lui dit.
type fauxUmami struct {
	*httptest.Server
	statut atomic.Int32
	recus  chan []byte
}

func nouveauFauxUmami(t *testing.T) *fauxUmami {
	t.Helper()
	f := &fauxUmami{recus: make(chan []byte, 64)}
	f.statut.Store(http.StatusOK)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/send" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "inattendu", http.StatusBadRequest)
			return
		}
		corps, _ := io.ReadAll(r.Body)
		f.recus <- corps
		w.WriteHeader(int(f.statut.Load()))
		_, _ = w.Write([]byte(`{"cache":"x"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// journal est un tampon qu'on peut lire pendant que le goroutine d'envoi écrit.
type journal struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (j *journal) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.Write(p)
}

func (j *journal) String() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.String()
}

func attend(t *testing.T, quoi string, cond func() bool) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatalf("toujours pas après 5 s : %s", quoi)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLEnvoiParleLaLangueDUmami(t *testing.T) {
	f := nouveauFauxUmami(t)
	a, err := Nouveau(f.URL+"/", siteTest, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, arret := context.WithCancel(context.Background())
	defer arret()
	go a.Tourne(ctx)

	sert(a.Mesure(routeur()), formulaire("https://releve.fcazals.fr/semaine/affectation"))

	var corps []byte
	select {
	case corps = <-f.recus:
	case <-time.After(5 * time.Second):
		t.Fatal("rien n'est arrivé chez Umami")
	}
	var e struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(corps, &e); err != nil {
		t.Fatalf("corps illisible : %v — %s", err, corps)
	}
	if e.Type != "event" {
		t.Errorf("type = %q", e.Type)
	}
	// Les noms de champs sont ceux que lit /api/send d'Umami v3 : un nom faux y
	// est ignoré sans erreur, et la mesure arrive amputée.
	for champ, attendu := range map[string]any{
		"website":   siteTest,
		"hostname":  "releve.fcazals.fr",
		"url":       "/semaine/affectation",
		"name":      "POST /semaine/affectation",
		"ip":        "203.0.113.8",
		"userAgent": "Mozilla/5.0 (X11; Linux x86_64) Firefox/140.0",
	} {
		if e.Payload[champ] != attendu {
			t.Errorf("payload.%s = %v, attendu %v", champ, e.Payload[champ], attendu)
		}
	}
	// Umami ne prend « timestamp » qu'en secondes entières : l'envoyer
	// mélangeait l'ordre des pages d'une même seconde.
	if _, ok := e.Payload["timestamp"]; ok {
		t.Errorf("payload.timestamp envoyé : %s", corps)
	}
}

func TestUmamiEnPanneNeCassePasLaPageEtLeDitUneFois(t *testing.T) {
	f := nouveauFauxUmami(t)
	f.statut.Store(http.StatusServiceUnavailable)
	var j journal
	a, err := Nouveau(f.URL, siteTest, slog.New(slog.NewTextHandler(&j, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, arret := context.WithCancel(context.Background())
	defer arret()
	go a.Tourne(ctx)

	h := a.Mesure(routeur())
	for range 3 {
		if rec := sert(h, navigation("https://releve.fcazals.fr/semaine")); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("la page a changé avec Umami en panne : %d %q", rec.Code, rec.Body.String())
		}
		<-f.recus
	}
	attend(t, "trois mesures perdues", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.perdues == 3
	})
	if n := strings.Count(j.String(), "mesure d'audience perdue"); n != 1 {
		t.Errorf("la panne est dite %d fois, attendu une seule :\n%s", n, j.String())
	}

	f.statut.Store(http.StatusOK)
	sert(h, navigation("https://releve.fcazals.fr/semaine"))
	<-f.recus
	attend(t, "le retour d'Umami dit dans le journal", func() bool {
		return strings.Contains(j.String(), "mesure d'audience rétablie") && strings.Contains(j.String(), "perdues=3")
	})
}

func TestUneFilePleineNAttendPas(t *testing.T) {
	var j journal
	a, err := Nouveau("https://umami.exemple.fr", siteTest, slog.New(slog.NewTextHandler(&j, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Personne ne vide la file : c'est Umami qui ne répond plus.
	h := a.Mesure(routeur())
	debut := time.Now()
	for range capaciteFile + 10 {
		sert(h, navigation("https://releve.fcazals.fr/semaine"))
	}
	if d := time.Since(debut); d > 2*time.Second {
		t.Errorf("%d pages en %v : la mesure a fait attendre les réponses", capaciteFile+10, d)
	}
	if a.perdues != 10 {
		t.Errorf("perdues = %d, attendu 10", a.perdues)
	}
	if n := strings.Count(j.String(), "mesure d'audience perdue"); n != 1 {
		t.Errorf("la file pleine est dite %d fois, attendu une seule", n)
	}
}

// Les apps qui reçoivent des photos repoussent l'échéance de lecture par
// http.ResponseController : à travers le temoin, elle doit rester joignable.
func TestLeTemoinLaissePasserLeControleDeLaReponse(t *testing.T) {
	a := allumee(t)
	errs := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /photos", func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		err := rc.SetReadDeadline(time.Now().Add(time.Minute))
		w.WriteHeader(http.StatusSeeOther)
		if err == nil {
			err = rc.Flush()
		}
		errs <- err
	})
	srv := httptest.NewServer(a.Mesure(mux))
	defer srv.Close()

	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/photos", strings.NewReader("x"))
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", "Mozilla/5.0")
	rep, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = rep.Body.Close()
	if err := <-errs; err != nil {
		t.Errorf("ResponseController à travers le temoin : %v", err)
	}
	if rep.StatusCode != http.StatusSeeOther {
		t.Errorf("code = %d, attendu 303", rep.StatusCode)
	}
	// La mesure est notée APRÈS le retour du handler, quand le client a déjà
	// sa réponse : Close attend la fin de la requête côté serveur.
	srv.Close()
	if n := len(a.file); n != 1 {
		t.Errorf("%d mesure(s), attendu 1 : la requête passait bien par le temoin", n)
	}
}

func TestDemarreDitLEtatDeLaMesure(t *testing.T) {
	for _, c := range []struct{ nom, adresse, site, dit string }{
		{"éteinte", "", "", "mesure d'audience éteinte"},
		{"illisible", "https://umami.fcazals.fr", "", "mesure d'audience ÉTEINTE : configuration illisible"},
		{"active", "https://umami.fcazals.fr", siteTest, "mesure d'audience active"},
	} {
		t.Run(c.nom, func(t *testing.T) {
			var j journal
			a := Demarre(c.adresse, c.site, slog.New(slog.NewTextHandler(&j, nil)))
			if !strings.Contains(j.String(), c.dit) {
				t.Errorf("journal = %q, attendu %q", j.String(), c.dit)
			}
			if a.Active() != (c.nom == "active") {
				t.Errorf("Active() = %v", a.Active())
			}
		})
	}
}
