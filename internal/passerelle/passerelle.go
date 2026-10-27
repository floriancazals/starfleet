// Package passerelle relie backup à son sous-paquet expedition, et à rien
// d'autre : un paquet internal ne s'importe que depuis starfleet, donc aucune app
// ne peut s'en servir pour brancher un envoi sur son Runner.
package passerelle

import "context"

// Envoi pose une copie scellée quelque part hors de la machine.
type Envoi func(ctx context.Context, nom string, scelle []byte) error

// Expedition branche un envoi sur un *backup.Runner. Le paquet backup la pose à
// son initialisation ; seul backup/expedition l'appelle.
var Expedition func(runner any, envoi Envoi)
