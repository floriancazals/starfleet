package backup

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// banc monte un service sur le disque : une base qui référence des documents, et
// le dossier où ces documents vivent.
type banc struct {
	db      liveDB
	docsDir string
	cfg     Config
}

func nouveauBanc(t *testing.T) *banc {
	t.Helper()
	dir := t.TempDir()
	live := newLive(t, 1)
	if _, err := live.db.Exec(`CREATE TABLE document (id INTEGER PRIMARY KEY, fichier TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	b := &banc{db: live, docsDir: filepath.Join(dir, "documents")}
	b.cfg = Config{
		Dir:    filepath.Join(dir, "backups"),
		Prefix: "lease",
		Keep:   3,
		Magic:  "LOTBAK1\n",
		Lot: &Lot{
			Base:       "lease.db",
			Dossiers:   []Dossier{{Membre: "documents/", Chemin: b.docsDir}},
			References: Colonne(ColonneRef{Membre: "documents/", Table: "document", Colonne: "fichier"}),
		},
	}
	return b
}

// emet ajoute une ligne de document et, si avecFichier, écrit son fichier.
func (b *banc) emet(t *testing.T, nom string, avecFichier bool) {
	t.Helper()
	if avecFichier {
		chemin := filepath.Join(b.docsDir, filepath.FromSlash(nom))
		if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(chemin, []byte("%PDF-1.3 "+nom), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.db.db.Exec(`INSERT INTO document (fichier) VALUES (?)`, nom); err != nil {
		t.Fatal(err)
	}
}

// membres liste les membres d'une archive, dans l'ordre.
func membres(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("lecture de l'archive : %v", err)
		}
		out = append(out, h.Name)
	}
}

func TestLotPorteLaBaseEtLesDossiers(t *testing.T) {
	b := nouveauBanc(t)
	b.emet(t, "bail-1/quittance-2026-03.pdf", true)
	b.emet(t, "bail-1/quittance-2026-04.pdf", true)

	snap, err := b.cfg.Take(context.Background(), b.db, day(0))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Fichiers != 2 {
		t.Errorf("Fichiers = %d, attendu 2", snap.Fichiers)
	}
	if base := filepath.Base(snap.Path); base != "lease-20260820T045000.000Z.tar" {
		t.Errorf("nom = %q, attendu un .tar horodaté", base)
	}
	noms := membres(t, snap.Path)
	// La base d'abord : c'est l'ordre qui garantit qu'un fichier référencé en base
	// est déjà sur le disque quand la marche l'atteint.
	if len(noms) != 3 || noms[0] != "lease.db" {
		t.Fatalf("membres = %v, attendu la base puis les deux documents", noms)
	}
	for _, n := range noms[1:] {
		if !strings.HasPrefix(n, "documents/bail-1/") {
			t.Errorf("membre inattendu : %q", n)
		}
	}
	if err := b.cfg.Verify(snap.Path); err != nil {
		t.Errorf("Verify : %v", err)
	}
	entrees, _ := os.ReadDir(b.cfg.Dir)
	for _, e := range entrees {
		if strings.Contains(e.Name(), ".part") || strings.Contains(e.Name(), ".verify") {
			t.Errorf("fichier temporaire laissé derrière : %s", e.Name())
		}
	}
}

// Une archive dont la base référence un fichier absent est refusée, et rien
// n'est laissé dans le dossier.
func TestLotRefuseUnFichierAbsent(t *testing.T) {
	b := nouveauBanc(t)
	b.emet(t, "a.pdf", true)
	b.emet(t, "jamais-ecrit.pdf", false)

	_, err := b.cfg.Take(context.Background(), b.db, day(0))
	if err == nil || !strings.Contains(err.Error(), "documents/jamais-ecrit.pdf") || !strings.Contains(err.Error(), "absent de l'archive") {
		t.Fatalf("Take = %v, attendu un refus qui nomme le fichier absent", err)
	}
	entrees, _ := os.ReadDir(b.cfg.Dir)
	for _, e := range entrees {
		t.Errorf("fichier laissé derrière : %s", e.Name())
	}
}

func TestLotRefuseUneArchiveTronquee(t *testing.T) {
	b := nouveauBanc(t)
	b.emet(t, "a.pdf", true)
	snap, err := b.cfg.Take(context.Background(), b.db, day(0))
	if err != nil {
		t.Fatal(err)
	}
	octets, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatal(err)
	}
	tronque := filepath.Join(t.TempDir(), "tronque.tar")
	if err := os.WriteFile(tronque, octets[:len(octets)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.cfg.Verify(tronque); err == nil {
		t.Error("une archive tronquée doit être refusée")
	}
}

func TestLotRefuseUneArchiveSansBase(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "sansbase.tar")
	f, err := os.Create(chemin)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	contenu := []byte("pas une base")
	if err := tw.WriteHeader(&tar.Header{Name: "documents/x.pdf", Mode: 0o600, Size: int64(len(contenu))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(contenu); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = nouveauBanc(t).cfg.Verify(chemin)
	if err == nil || !strings.Contains(err.Error(), "lease.db") {
		t.Errorf("Verify = %v, attendu un refus qui nomme lease.db", err)
	}
}

// Une instance neuve n'a encore rien émis : son dossier n'existe pas, et le lot
// se prend quand même.
func TestLotSansDossier(t *testing.T) {
	b := nouveauBanc(t)
	snap, err := b.cfg.Take(context.Background(), b.db, day(0))
	if err != nil {
		t.Fatalf("un dossier absent ne doit pas empêcher la sauvegarde : %v", err)
	}
	if snap.Fichiers != 0 {
		t.Errorf("Fichiers = %d, attendu 0", snap.Fichiers)
	}
}

// Une table qui n'existe pas encore dans la copie est sautée : un lot pris avant
// elle reste un lot valide de son époque.
func TestColonneSauteUneTableAbsente(t *testing.T) {
	b := nouveauBanc(t)
	b.cfg.Lot.References = Colonne(
		ColonneRef{Membre: "documents/", Table: "document", Colonne: "fichier"},
		ColonneRef{Membre: "fichiers/", Table: "fichier", Colonne: "chemin"},
	)
	b.emet(t, "a.pdf", true)
	if _, err := b.cfg.Take(context.Background(), b.db, day(0)); err != nil {
		t.Fatalf("une table absente doit être sautée : %v", err)
	}
}

func TestLotSeRestaureEnDossier(t *testing.T) {
	b := nouveauBanc(t)
	b.emet(t, "bail-1/q.pdf", true)
	key := testKey(t)
	snap, err := b.cfg.Take(context.Background(), b.db, day(0))
	if err != nil {
		t.Fatal(err)
	}
	scelle := filepath.Join(t.TempDir(), SealedName(snap.Path))
	if err := b.cfg.SealFile(key, snap.Path, scelle); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "rendu")
	if err := b.cfg.Restore(key, scelle, dest); err != nil {
		t.Fatal(err)
	}
	base, dossiers := b.cfg.Restored(dest)
	if err := verifyDB(base); err != nil {
		t.Errorf("la base rendue ne s'ouvre pas : %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dossiers["documents/"], "bail-1", "q.pdf"))
	if err != nil || string(got) != "%PDF-1.3 bail-1/q.pdf" {
		t.Errorf("document rendu = %q (%v)", got, err)
	}
	if err := b.cfg.Restore(key, scelle, dest); err == nil {
		t.Error("une restauration ne doit jamais écrire dans un dossier existant")
	}
}

func TestCheminRefuseUneEvasion(t *testing.T) {
	dest := t.TempDir()
	for _, m := range []string{"../x", "/etc/passwd", "a/../../x", ".."} {
		if _, err := chemin(dest, m); err == nil {
			t.Errorf("chemin(%q) accepté : un membre ne doit pas sortir du dossier", m)
		}
	}
	if got, err := chemin(dest, "documents/a.pdf"); err != nil || got != filepath.Join(dest, "documents", "a.pdf") {
		t.Errorf("chemin = %q (%v)", got, err)
	}
}

// Une copie d'un ancien format de l'app est nommée, pas confondue avec un
// fichier abîmé.
func TestUnAncienFormatEstNomme(t *testing.T) {
	key := testKey(t)
	ancien := Config{Prefix: "ateliers", Magic: "ATLBAK1\n"}
	blob, err := ancien.Seal(key, []byte("une base d'avant les lots"))
	if err != nil {
		t.Fatal(err)
	}
	actuel := Config{Prefix: "ateliers", Magic: "ATLBAK2\n", Anciens: map[string]string{
		"ATLBAK1\n": "copie scellée d'avant les lots : elle se restaure avec un binaire de cette époque",
	}}
	if _, err := actuel.Unseal(key, blob); err == nil || !strings.Contains(err.Error(), "d'avant les lots") {
		t.Errorf("Unseal = %v, attendu l'explication de l'ancien format", err)
	}
}

// L'en-tête est propre à chaque app : la copie d'une voisine est refusée, même
// avec la bonne clé — la flotte n'en a qu'une.
func TestLaCopieDUneVoisineEstRefusee(t *testing.T) {
	key := testKey(t)
	blob, err := Config{Magic: "TODOBAK1\n"}.Seal(key, []byte("la base de todo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Unseal(key, blob); err == nil || !strings.Contains(err.Error(), "en-tête inconnu") {
		t.Errorf("Unseal = %v, attendu un refus d'en-tête", err)
	}
}

func TestUneConfigSansEnTeteNeScelleRien(t *testing.T) {
	key := testKey(t)
	if _, err := (Config{}).Seal(key, []byte("x")); err == nil {
		t.Error("sans Magic, une copie scellée n'aurait pas d'en-tête : refuser")
	}
	if _, err := (Config{Dir: t.TempDir(), Prefix: "x"}).Take(context.Background(), newLive(t, 1), time.Now()); err == nil {
		t.Error("sans Magic, Take doit refuser avant d'écrire quoi que ce soit")
	}
	lot := Config{Dir: t.TempDir(), Prefix: "x", Magic: "X\n", Lot: &Lot{}}
	if _, err := lot.Take(context.Background(), newLive(t, 1), time.Now()); err == nil {
		t.Error("un lot sans nom de base doit être refusé")
	}
}

// Colonne lit la bonne colonne, préfixe compris, et ignore les lignes vides.
func TestColonneLitLesReferences(t *testing.T) {
	b := nouveauBanc(t)
	b.emet(t, "a.pdf", false)
	if _, err := b.db.db.Exec(`INSERT INTO document (fichier) VALUES ('')`); err != nil {
		t.Fatal(err)
	}
	refs, err := b.cfg.Lot.References(b.db.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Membre != "documents/a.pdf" || refs[0].Table != "document" {
		t.Errorf("références = %+v", refs)
	}
}
