package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/floriancazals/starfleet/internal/passerelle"
)

// fauxSalon tient lieu du salon où backup/expedition pose la copie scellée.
type fauxSalon struct {
	noms  []string
	blobs [][]byte
	err   error
}

func (s *fauxSalon) envoie(_ context.Context, nom string, scelle []byte) error {
	if s.err != nil {
		return s.err
	}
	s.noms = append(s.noms, nom)
	s.blobs = append(s.blobs, bytes.Clone(scelle))
	return nil
}

// expedie branche le salon sur le Runner, comme le fait backup/expedition.
func expedie(r *Runner, s *fauxSalon) { passerelle.Expedition(r, s.envoie) }

// Ce qui part est scellé, et redevient une vraie base une fois descellé.
func TestCeQuiPartEstScelleEtRestaurable(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	salon := &fauxSalon{}
	r := newRunner(t, dir, newLive(t, 5), nil)
	r.Key = key
	expedie(r, salon)

	r.tick(context.Background(), at(0, 4, 50))
	if len(salon.blobs) != 1 {
		t.Fatalf("%d copies envoyées, attendu 1", len(salon.blobs))
	}
	if !strings.HasSuffix(salon.noms[0], ".db.gz.enc") {
		t.Errorf("nom = %q, attendu celui d'une copie scellée", salon.noms[0])
	}
	// L'en-tête de tout fichier SQLite : s'il apparaît ici, la base est partie en clair.
	if bytes.Contains(salon.blobs[0], []byte("SQLite format 3")) || bytes.Contains(salon.blobs[0], []byte(nomFournisseur)) {
		t.Fatal("la base est partie en clair")
	}
	sealed := filepath.Join(t.TempDir(), salon.noms[0])
	if err := os.WriteFile(sealed, salon.blobs[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Cfg.Restore(key, sealed, filepath.Join(t.TempDir(), "rendu.db")); err != nil {
		t.Errorf("ce qui est parti ne se restaure pas : %v", err)
	}
	// Un envoi réussi est noté : c'est lui que l'alerte d'âge surveille.
	if _, ok := LastPush(dir); !ok {
		t.Error("l'envoi n'a pas été noté")
	}
}

// Pas de clé, pas d'envoi : une base en clair ne doit jamais atterrir dans un
// salon, même au prix de n'avoir aucune copie hors machine.
func TestSansCleRienNePart(t *testing.T) {
	dir := t.TempDir()
	salon := &fauxSalon{}
	r := newRunner(t, dir, newLive(t, 2), nil)
	expedie(r, salon)

	r.tick(context.Background(), at(0, 4, 50))
	if all, _ := r.Cfg.List(); len(all) != 1 {
		t.Fatalf("copies locales : %v, attendu une — elles se font quand même", names(all))
	}
	if len(salon.blobs) != 0 || r.Ships() {
		t.Fatalf("%d copies envoyées sans clé", len(salon.blobs))
	}
	if err := r.Ship(context.Background(), Snapshot{}, at(0, 5, 0)); err == nil {
		t.Error("Ship doit refuser sans clé")
	}
}

// Un envoi raté reste au journal, sans alerte — c'est l'âge qui attrape un salon
// qui reste cassé — et la copie locale tient.
func TestUnEnvoiRateLaisseLaCopieEtSeTait(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	salon := &fauxSalon{err: os.ErrDeadlineExceeded}
	r := newRunner(t, dir, newLive(t, 2), n)
	r.Key = testKey(t)
	expedie(r, salon)

	r.tick(context.Background(), at(0, 4, 50))
	if all, _ := r.Cfg.List(); len(all) != 1 {
		t.Errorf("copies = %v, attendu la locale", names(all))
	}
	if len(n.texts()) != 0 {
		t.Errorf("alertes = %q, attendu aucune pour un seul envoi raté", n.texts())
	}
	if _, ok := LastPush(dir); ok {
		t.Error("un envoi raté ne doit pas être noté comme fait")
	}
	// C'est exactement ce qui fait sonner l'alerte d'âge deux jours plus tard.
	r.checkAge(context.Background(), at(3, 9, 0))
	if len(n.texts()) != 1 {
		t.Errorf("alertes = %q, attendu que le silence finisse par se dire", n.texts())
	}
}

// Les deux destinations sont tentées : un disque débranché ne prive pas le salon
// de sa copie.
func TestUnDisqueDebrancheNEmpechePasLEnvoi(t *testing.T) {
	dir := t.TempDir()
	salon := &fauxSalon{}
	r := newRunner(t, dir, newLive(t, 2), nil)
	r.Key = testKey(t)
	r.CopyDir = filepath.Join(t.TempDir(), "disque") // pas de témoin : non monté
	expedie(r, salon)

	r.tick(context.Background(), at(0, 4, 50))
	if len(salon.blobs) != 1 {
		t.Errorf("%d copies envoyées, attendu 1 malgré le disque absent", len(salon.blobs))
	}
}
