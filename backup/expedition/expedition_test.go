package expedition

import (
	"context"
	"testing"

	"github.com/floriancazals/starfleet/backup"
)

type depot struct {
	actif bool
	recus []string
}

func (d *depot) Enabled() bool { return d.actif }

func (d *depot) SendDocument(_ context.Context, filename string, _ []byte) error {
	d.recus = append(d.recus, filename)
	return nil
}

func TestBrancheUnDepotActif(t *testing.T) {
	r := &backup.Runner{Key: make([]byte, backup.KeySize)}
	if r.Ships() {
		t.Fatal("un Runner neuf ne doit rien envoyer")
	}
	Branche(r, &depot{actif: true})
	if !r.Ships() {
		t.Error("un dépôt actif doit brancher l'envoi")
	}
}

// Un dépôt éteint (pas de jeton, pas de salon) ne branche rien : l'âge se mesure
// alors sur la copie locale ou le disque, pas sur un envoi qui n'aura jamais lieu.
func TestUnDepotEteintNeBrancheRien(t *testing.T) {
	r := &backup.Runner{Key: make([]byte, backup.KeySize)}
	Branche(r, &depot{actif: false})
	Branche(r, nil)
	Branche(nil, &depot{actif: true})
	if r.Ships() {
		t.Error("un dépôt éteint ne doit rien brancher")
	}
}
