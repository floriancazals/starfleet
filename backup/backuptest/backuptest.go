// Package backuptest aide les apps à tester ce que starfleet/backup leur promet.
package backuptest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// expedition est le seul chemin par lequel une base part sur le réseau.
const expedition = "github.com/floriancazals/starfleet/backup/expedition"

// AucuneExpedition échoue si le module qui contient le dossier courant importe
// backup/expedition ou construit un notify.Depot, n'importe où dans son code.
//
// C'est le test des apps dont la base n'est pas celle de leur auteur — gate et
// ses comptes, ateliers et ses notes cliniques, les apps des clients : leur base
// n'a aucun chemin vers un salon, et ce n'est pas une question de configuration.
// Le jour où quelqu'un branche un envoi, ce test rougit et fait relire pourquoi
// il n'y en avait pas.
func AucuneExpedition(tb testing.TB) {
	tb.Helper()
	racine, err := racineDuModule()
	if err != nil {
		tb.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(racine, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != racine && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(racine, path)
		notify := ""
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == expedition {
				tb.Errorf("%s importe %s : la base de cette app ne doit avoir aucun chemin vers un salon", rel, p)
			}
			if p == "github.com/floriancazals/starfleet/notify" {
				notify = "notify"
				if imp.Name != nil {
					notify = imp.Name.Name
				}
			}
		}
		if notify == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == notify && (sel.Sel.Name == "NewDepot" || sel.Sel.Name == "Depot") {
				tb.Errorf("%s : %s.%s fait partir des fichiers dans un salon, et cette app ne doit pas en avoir",
					rel, notify, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// racineDuModule remonte du dossier courant jusqu'au go.mod.
func racineDuModule() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
