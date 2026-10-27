package backuptest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// enregistreur note les échecs au lieu de faire échouer le test qui l'utilise.
type enregistreur struct {
	testing.TB
	erreurs []string
}

func (e *enregistreur) Helper() {}

func (e *enregistreur) Errorf(format string, args ...any) {
	e.erreurs = append(e.erreurs, fmt.Sprintf(format, args...))
}

func (e *enregistreur) Fatal(args ...any) { e.erreurs = append(e.erreurs, fmt.Sprint(args...)) }

// module écrit un faux module dans un dossier temporaire et s'y place.
func module(t *testing.T, fichiers map[string]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module exemple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for nom, contenu := range fichiers {
		p := filepath.Join(dir, nom)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contenu), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sous := filepath.Join(dir, "cmd", "app")
	if err := os.MkdirAll(sous, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sous)
}

func TestUneAppSansExpeditionPasse(t *testing.T) {
	module(t, map[string]string{
		"cmd/app/main.go": `package main

import "github.com/floriancazals/starfleet/notify"

func main() { _ = notify.NewTelegram("", "") }
`,
	})
	e := &enregistreur{TB: t}
	AucuneExpedition(e)
	if len(e.erreurs) != 0 {
		t.Errorf("erreurs = %q, attendu aucune", e.erreurs)
	}
}

func TestUneAppQuiImporteExpeditionEchoue(t *testing.T) {
	module(t, map[string]string{
		"cmd/app/backup.go": `package main

import _ "github.com/floriancazals/starfleet/backup/expedition"
`,
	})
	e := &enregistreur{TB: t}
	AucuneExpedition(e)
	if len(e.erreurs) != 1 {
		t.Errorf("erreurs = %q, attendu un refus de l'import", e.erreurs)
	}
}

func TestUneAppQuiConstruitUnDepotEchoue(t *testing.T) {
	module(t, map[string]string{
		"interne/salon.go": `package interne

import tg "github.com/floriancazals/starfleet/notify"

var D = tg.NewDepot("jeton", "1")
`,
	})
	e := &enregistreur{TB: t}
	AucuneExpedition(e)
	if len(e.erreurs) != 1 {
		t.Errorf("erreurs = %q, attendu un refus du dépôt, alias compris", e.erreurs)
	}
}
