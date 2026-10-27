// Package backup fait, vérifie et range les copies datées de la base SQLite
// d'une app — et, pour celles qui gardent des fichiers à côté de leur base, de
// ces fichiers aussi.
//
// Quatre idées portent ce paquet, et ce sont celles à garder s'il est un jour
// réécrit :
//
//  1. La copie est faite par SQLite lui-même, avec VACUUM INTO — jamais par une
//     copie de fichier. En mode WAL les données vivent dans deux fichiers (la base
//     et son -wal, plus un index -shm), et un `cp` de la première pendant qu'une
//     écriture est en vol rend un fichier qui s'ouvre très bien et auquel il
//     manque des lignes. VACUUM INTO tourne dans une transaction de lecture et
//     écrit une seule base, déjà compactée, sans WAL.
//  2. La copie est vérifiée AVANT d'avoir le droit de prendre son nom définitif,
//     et une copie refusée ne remplace jamais la précédente. Une corruption
//     silencieuse qui tourne est pire que pas de sauvegarde : elle coûte le seul
//     bon fichier qui restait.
//  3. Ce qui quitte la machine est scellé (gzip puis AES-256-GCM), et la clé ne
//     vit pas sur cette machine. Une sauvegarde existe pour le jour où le disque
//     n'est plus là ; une clé qui n'aurait vécu que là serait partie avec lui.
//  4. Ce paquet n'a AUCUN chemin réseau. La copie scellée va dans un dossier
//     (BACKUP_COPY_DIR, le disque externe ou la salle d'embarquement que le Pi 5
//     vient vider, voir copy.go), et l'alerte ne porte que du texte (Alerter).
//     Les apps dont les données sont celles de leur auteur peuvent en plus poser
//     la copie scellée dans un salon Telegram : cela passe par le sous-paquet
//     expedition, et SEULEMENT par lui. Une app qui ne l'importe pas — gate, les
//     apps des clients, ateliers et ses notes cliniques — n'a aucun moyen de faire
//     partir sa base, et aucune variable d'environnement ne peut lui en donner un.
//
// Deux formes d'artefact, choisies par Config.Lot :
//
//   - sans Lot, la copie est la base seule (« traiteur-…Z.db ») ;
//   - avec un Lot, c'est UN tar qui porte la base ET les dossiers de fichiers
//     qu'elle référence (« lease-…Z.tar ») — une quittance dont le PDF manque est
//     une ligne qui ne parle de rien. La vérification refuse un lot dont la base
//     référence un fichier que l'archive ne porte pas.
//
// Chaque app a son propre en-tête de copie scellée (Config.Magic). Il fait partie
// du format : le changer rendrait illisibles toutes les copies déjà écrites.
//
// Il n'y a délibérément aucun binaire sqlite3 ni tar là-dedans : les images
// d'exécution sont distroless et ne contiennent qu'un exécutable — le service.
// La copie passe par le pilote pur-Go déjà lié dedans, ce qui est aussi la raison
// pour laquelle -snapshot vit dans le même binaire que le serveur HTTP.
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // le pilote pur-Go des apps, pour vérifier une copie
)

const (
	// stamp date le nom d'une copie, en UTC, à la milliseconde, avec un Z explicite.
	//
	// UTC parce que les noms sont triés comme du texte pour ordonner les copies :
	// un horodatage local inverserait deux d'entre elles pendant l'heure d'automne
	// qui a lieu deux fois, et le ménage effacerait la mauvaise. À la milliseconde
	// parce qu'un horodatage à la seconde faisait se heurter deux passes dans la
	// même seconde — et la première chose qu'on fait après un -push refusé, c'est
	// le relancer. La largeur est fixe : l'ordre du texte reste celui du temps.
	stamp = "20060102T150405.000"

	// sealedExt suit le nom d'une copie dans celui de sa forme scellée.
	sealedExt = ".gz.enc"

	// KeySize est la longueur d'une clé de sauvegarde en octets (AES-256).
	KeySize = 32
)

// ErrNoSnapshot est rendue par Latest quand le dossier n'en contient aucune.
var ErrNoSnapshot = errors.New("aucune sauvegarde pour l'instant")

// Snapshotter est ce dont Take a besoin de la base vivante : demander à SQLite
// lui-même une copie cohérente (VACUUM INTO). Le *sqlite.Store de chaque app
// l'implémente.
type Snapshotter interface {
	SnapshotInto(ctx context.Context, path string) error
}

// Config dit où vivent les copies, combien on en garde, et sous quelle forme.
type Config struct {
	Dir    string // dossier des copies locales
	Prefix string // préfixe des noms de fichier, par exemple « traiteur » (voir PrefixFromPath)
	Keep   int    // copies gardées ; zéro ou moins les garde toutes

	// Magic est l'en-tête des copies scellées de CETTE app (« TRAIBAK1\n »). Il
	// sert aussi de donnée authentifiée du chiffrement : une copie ne peut pas se
	// faire passer pour une autre version du format. Il ne change jamais.
	Magic string
	// Anciens nomme les en-têtes d'anciens formats de l'app, avec l'explication
	// donnée à qui tente de les restaurer avec ce binaire-ci.
	Anciens map[string]string

	// Lot, s'il est posé, fait de chaque copie un tar qui porte aussi des fichiers.
	Lot *Lot
}

// Lot décrit ce qu'une copie porte à côté de la base.
type Lot struct {
	// Base est le nom de la base dans l'archive, par exemple « lease.db ».
	Base string
	// Dossiers sont les dossiers portés après la base.
	Dossiers []Dossier
	// References rend les membres de l'archive que la base nomme, lue dans la
	// copie extraite. Un lot qui n'en porte pas un est refusé. nil : aucun
	// contrôle au-delà de la base elle-même.
	References func(db *sql.DB) ([]Reference, error)
}

// Dossier est un dossier du disque porté dans le lot sous un préfixe.
type Dossier struct {
	Membre string // préfixe dans l'archive, par exemple « documents/ »
	Chemin string // dossier sur le disque
}

// Reference est un fichier que la base nomme et que le lot doit porter.
type Reference struct {
	Membre string // nom complet dans l'archive, préfixe compris
	Table  string // d'où vient la référence, pour le message d'erreur
}

// Snapshot est une copie vérifiée posée sur le disque.
type Snapshot struct {
	Path string
	Size int64
	// Fichiers est le nombre de fichiers portés à côté de la base (lot seulement).
	Fichiers int
	// Taken est l'instant de la copie, lu dans le nom du fichier plutôt que dans
	// sa date de modification : le nom est la seule chose qui survit à une copie,
	// un rsync ou une restauration. Pour un nom que ce paquet n'a pas écrit, c'est
	// la date de modification.
	Taken time.Time
}

// Age dit depuis combien de temps la copie a été prise.
func (s Snapshot) Age(now time.Time) time.Duration { return now.Sub(s.Taken) }

// PrefixFromPath tire le préfixe des copies du chemin de la base vivante :
// /data/traiteur.db → « traiteur ».
func PrefixFromPath(dbPath string) string {
	base := filepath.Base(dbPath)
	if base == "." || base == string(filepath.Separator) {
		return "db"
	}
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "db"
	}
	return base
}

// suffix termine le nom d'une copie : « Z.db » pour la base seule, « Z.tar »
// pour un lot. Le Z est celui de l'horodatage.
func (c Config) suffix() string {
	if c.Lot != nil {
		return "Z.tar"
	}
	return "Z.db"
}

func (c Config) sealedSuffix() string { return c.suffix() + sealedExt }

// check refuse une configuration qui écrirait des copies impossibles à relire.
func (c Config) check() error {
	if c.Dir == "" {
		return errors.New("sauvegarde : aucun dossier configuré")
	}
	return c.checkMagic()
}

func (c Config) checkMagic() error {
	if c.Magic == "" {
		return errors.New("sauvegarde : Config.Magic est vide, une copie scellée n'aurait pas d'en-tête")
	}
	if c.Lot != nil && c.Lot.Base == "" {
		return errors.New("sauvegarde : Lot.Base est vide, la base n'aurait pas de nom dans l'archive")
	}
	return nil
}

// Take écrit une copie vérifiée dans c.Dir et la rend. Elle ne fait PAS le
// ménage : la rétention est un second geste, à part (Prune), parce qu'un dossier
// qui porte une copie de trop n'est pas une sauvegarde ratée et ne doit pas être
// signalé comme tel — une erreur d'ici veut toujours dire « pas de copie neuve ».
//
// Rien n'est détruit avant que la copie neuve ait passé la vérification : une
// passe ratée laisse le dossier exactement comme il était, et c'est tout l'objet
// du nom temporaire ci-dessous.
func (c Config) Take(ctx context.Context, db Snapshotter, now time.Time) (Snapshot, error) {
	if err := c.check(); err != nil {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return Snapshot{}, fmt.Errorf("dossier de sauvegarde : %w", err)
	}
	final := filepath.Join(c.Dir, c.name(now))
	part := final + ".part"

	// Un .part laissé par une passe tuée doit partir : VACUUM INTO refuse d'écrire
	// sur un fichier existant, et la passe d'après un plantage échouerait à vie.
	if err := removeDB(part); err != nil {
		return Snapshot{}, fmt.Errorf("effacement de %s : %w", filepath.Base(part), err)
	}
	fichiers := 0
	if c.Lot != nil {
		n, err := c.writeLot(ctx, db, part)
		if err != nil {
			_ = removeDB(part)
			return Snapshot{}, err
		}
		fichiers = n
	} else if err := db.SnapshotInto(ctx, part); err != nil {
		_ = removeDB(part)
		return Snapshot{}, err
	}
	if err := c.Verify(part); err != nil {
		_ = removeDB(part)
		return Snapshot{}, fmt.Errorf("copie refusée : %w", err)
	}
	// Dernier garde-fou. À la milliseconde, deux passes ne se heurtent pour ainsi
	// dire jamais, mais une horloge figée ou un appelant qui passe deux fois le
	// même instant laisserait Rename écraser : un seul fichier sur le disque alors
	// que les deux commandes ont imprimé un chemin et rendu zéro.
	if err := CheckTarget(final); err != nil {
		_ = removeDB(part)
		return Snapshot{}, err
	}
	if err := os.Rename(part, final); err != nil {
		_ = removeDB(part)
		return Snapshot{}, fmt.Errorf("nommage de la copie : %w", err)
	}
	st, err := os.Stat(final)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Path: final, Size: st.Size(), Fichiers: fichiers, Taken: now.UTC().Truncate(time.Millisecond)}, nil
}

// Verify vérifie une copie posée sur le disque : la base passe
// PRAGMA integrity_check et porte un schéma ; pour un lot, l'archive se lit
// jusqu'au bout et porte chaque fichier que la base référence.
func (c Config) Verify(path string) error {
	if c.Lot != nil {
		return c.verifyLot(path)
	}
	return verifyDB(path)
}

// verifyDB ouvre une base comme SQLite la trouvera et lance integrity_check. Un
// fichier qui n'est pas une base échoue ici aussi, à la première requête.
func verifyDB(path string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return checkDB(db)
}

func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
}

func checkDB(db *sql.DB) error {
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check : %s", res)
	}
	// Une base vide est une base parfaitement valide, donc integrity_check seul
	// bénirait une copie de rien — exactement l'allure d'une copie tronquée ou
	// mal dirigée. On exige un schéma.
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return errors.New("aucune table : ce n'est pas une copie de cette app")
	}
	return nil
}

// List rend les copies de c.Dir, de la plus ancienne à la plus récente. L'ordre
// vient des noms, qui se trient dans l'ordre du temps (voir stamp).
func (c Config) List() ([]Snapshot, error) {
	return list(c.Dir, c.Prefix, c.suffix())
}

// ListSealed rend les copies scellées d'un dossier de copie, de la plus ancienne
// à la plus récente.
func (c Config) ListSealed(dir string) ([]Snapshot, error) {
	return list(dir, c.Prefix, c.sealedSuffix())
}

func list(dir, prefix, suffix string) ([]Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Snapshot
	for _, e := range entries {
		if e.IsDir() || !isSnapshotName(e.Name(), prefix, suffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		taken, ok := timeFromName(e.Name(), prefix, suffix)
		if !ok {
			taken = info.ModTime()
		}
		out = append(out, Snapshot{Path: filepath.Join(dir, e.Name()), Size: info.Size(), Taken: taken})
	}
	slices.SortFunc(out, func(a, b Snapshot) int {
		return strings.Compare(filepath.Base(a.Path), filepath.Base(b.Path))
	})
	return out, nil
}

// Latest rend la copie la plus récente de c.Dir, ou ErrNoSnapshot.
func (c Config) Latest() (Snapshot, error) {
	all, err := c.List()
	if err != nil {
		return Snapshot{}, err
	}
	if len(all) == 0 {
		return Snapshot{}, ErrNoSnapshot
	}
	return all[len(all)-1], nil
}

// Prune efface les copies les plus anciennes jusqu'à n'en garder que c.Keep, et
// dit combien il en a effacé. Keep ≤ 0 garde tout.
func (c Config) Prune() (int, error) {
	return prune(c.Dir, c.Prefix, c.suffix(), c.Keep)
}

// PruneSealed fait de même pour les copies scellées d'un dossier de copie, qui a
// sa propre rétention, en général plus profonde : c'est sur le disque externe
// qu'on veut de l'historique.
func (c Config) PruneSealed(dir string, keep int) (int, error) {
	return prune(dir, c.Prefix, c.sealedSuffix(), keep)
}

func prune(dir, prefix, suffix string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	all, err := list(dir, prefix, suffix)
	if err != nil {
		return 0, err
	}
	if len(all) <= keep {
		return 0, nil
	}
	var errs []error
	removed := 0
	for _, s := range all[:len(all)-keep] {
		if err := removeDB(s.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// name fabrique le nom d'une copie : traiteur-20260911T040500.000Z.db.
func (c Config) name(t time.Time) string {
	return c.Prefix + "-" + t.UTC().Format(stamp) + c.suffix()
}

// isSnapshotName dit si une entrée du dossier est un des fichiers que ce paquet
// écrit. Le « .part » d'une passe en cours échoue ici : une copie non vérifiée
// n'est jamais listée, effacée, restaurée ni expédiée.
func isSnapshotName(entry, prefix, suffix string) bool {
	return strings.HasPrefix(entry, prefix+"-") && strings.HasSuffix(entry, suffix)
}

// timeFromName relit l'horodatage dans le nom d'une copie. Un nom que ce paquet
// n'a pas écrit — une copie d'avant les millisecondes, par exemple — ne se lit
// pas, et l'appelant se rabat sur la date de modification.
func timeFromName(entry, prefix, suffix string) (time.Time, bool) {
	if !isSnapshotName(entry, prefix, suffix) {
		return time.Time{}, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(entry, prefix+"-"), suffix)
	t, err := time.ParseInLocation(stamp, body, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// removeDB efface un fichier et les fichiers annexes que SQLite a pu laisser à
// côté. Un fichier absent n'est pas une erreur.
func removeDB(path string) error {
	var errs []error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
