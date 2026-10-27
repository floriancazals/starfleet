// Package expedition fait partir chaque jour la copie scellée d'une base dans un
// salon Telegram, en plus du disque de copie.
//
// C'est le SEUL chemin par lequel une base de la flotte peut quitter la machine
// par le réseau, et il est dans un paquet à part pour une raison : une app qui
// ne l'importe pas n'a aucun moyen de le faire, quoi que dise sa configuration.
// Ne l'importent que les apps dont les données sont celles de leur auteur (todo,
// resa, escale-breakfast, lease). gate, ateliers et les apps des clients ne
// l'importent jamais — leurs tests le vérifient sur leur graphe d'imports.
package expedition

import (
	"context"

	"github.com/floriancazals/starfleet/backup"
	"github.com/floriancazals/starfleet/internal/passerelle"
)

// Depot est le salon où la copie scellée est posée. *notify.Depot l'implémente.
type Depot interface {
	Enabled() bool
	SendDocument(ctx context.Context, filename string, data []byte) error
}

// Branche fait poser par r, chaque jour, sa copie scellée dans d. Un dépôt nil ou
// éteint ne branche rien : r garde seulement ses copies locales et son disque.
func Branche(r *backup.Runner, d Depot) {
	if r == nil || d == nil || !d.Enabled() {
		return
	}
	passerelle.Expedition(r, d.SendDocument)
}
