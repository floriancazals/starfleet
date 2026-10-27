package backup

import (
	"archive/tar"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// writeLot écrit le tar : la base d'abord, les dossiers ensuite, et rend le
// nombre de fichiers portés.
//
// Cet ordre est l'argument de justesse de tout le lot. Un fichier émis ne change
// plus (une quittance, une photo d'état des lieux) : ses dossiers ne font que
// gagner des fichiers, et un tar pris pendant que le service tourne ne peut pas
// se contredire. Copier la base D'ABORD garantit donc que chaque fichier qu'elle
// référence est déjà sur le disque quand la marche l'atteint. L'ordre inverse
// laisserait un fichier émis entre-temps être référencé par la base et absent du
// tar. Le reste inverse — un fichier dans le tar que la base ne connaît pas — est
// un orphelin sans conséquence.
func (c Config) writeLot(ctx context.Context, db Snapshotter, dest string) (int, error) {
	// La copie de la base va dans son propre fichier temporaire, parce que VACUUM
	// INTO écrit un fichier et pas un flux ; elle part dès qu'elle est dans le tar.
	tmpDB := dest + ".db"
	if err := removeDB(tmpDB); err != nil {
		return 0, fmt.Errorf("effacement de la copie temporaire : %w", err)
	}
	if err := db.SnapshotInto(ctx, tmpDB); err != nil {
		return 0, err
	}
	defer func() { _ = removeDB(tmpDB) }()

	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("création de l'archive : %w", err)
	}
	tw := tar.NewWriter(f)
	abandon := func(err error) (int, error) {
		_ = tw.Close()
		_ = f.Close()
		return 0, err
	}
	if err := addFile(tw, tmpDB, c.Lot.Base); err != nil {
		return abandon(err)
	}
	fichiers := 0
	for _, d := range c.Lot.Dossiers {
		n, err := addDossier(tw, d.Chemin, d.Membre)
		if err != nil {
			return abandon(err)
		}
		fichiers += n
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("fermeture de l'archive : %w", err)
	}
	// La fermeture est vérifiée : sur un disque plein, c'est là que l'échec
	// apparaît, et un Close non vérifié est la façon dont une archive tronquée
	// reçoit un bon nom.
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("écriture de l'archive : %w", err)
	}
	return fichiers, nil
}

// addDossier porte un dossier dans le tar sous son préfixe. Un dossier absent
// n'est pas une erreur : une instance neuve n'a encore rien émis ni photographié,
// et la refuser voudrait dire aucune sauvegarde avant le premier fichier.
func addDossier(tw *tar.Writer, dir, membre string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	count := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if err := addFile(tw, path, membre+filepath.ToSlash(rel)); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("archivage de %s : %w", dir, err)
	}
	return count, nil
}

// addFile copie un fichier dans le tar sous le nom de membre donné.
func addFile(tw *tar.Writer, path, member string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    member,
		Mode:    0o600,
		Size:    st.Size(),
		ModTime: st.ModTime(),
		Format:  tar.FormatPAX,
	}); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	// tar note la taille dans l'en-tête : un fichier qui a grandi ou rétréci entre
	// le Stat et la copie écrit un membre que personne ne relira. Mieux vaut le
	// dire ici que le découvrir pendant une restauration.
	if n != st.Size() {
		return fmt.Errorf("%s a changé de taille pendant l'archivage (%d octets sur %d)", member, n, st.Size())
	}
	return nil
}

// verifyLot ouvre un lot et vérifie les trois choses qui peuvent y manquer :
// l'archive est complète, la base qu'elle porte est une base de cette app, et
// chaque fichier que cette base référence est porté.
func (c Config) verifyLot(path string) error {
	membres, tmpDB, err := c.extraire(path)
	if tmpDB != "" {
		defer func() { _ = removeDB(tmpDB) }()
	}
	if err != nil {
		return err
	}
	if tmpDB == "" {
		return fmt.Errorf("archive sans membre %s : ce n'est pas une sauvegarde de cette app", c.Lot.Base)
	}
	db, err := openDB(tmpDB)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := checkDB(db); err != nil {
		return err
	}
	if c.Lot.References == nil {
		return nil
	}
	refs, err := c.Lot.References(db)
	if err != nil {
		return fmt.Errorf("lecture des fichiers référencés : %w", err)
	}
	for _, r := range refs {
		if _, ok := membres[r.Membre]; !ok {
			return fmt.Errorf("le fichier %q est référencé en base (%s) et absent de l'archive", r.Membre, r.Table)
		}
	}
	return nil
}

// extraire lit tout le tar — c'est ce qui attrape une archive tronquée —, en
// relève les noms de membres et écrit la base dans un fichier temporaire à côté
// de l'archive. L'appelant efface ce fichier.
func (c Config) extraire(path string) (map[string]struct{}, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	membres := make(map[string]struct{})
	tmpDB := ""
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return membres, tmpDB, fmt.Errorf("archive illisible (tronquée ?) : %w", err)
		}
		membres[h.Name] = struct{}{}
		if h.Name != c.Lot.Base {
			// Lire le membre quand même : c'est ce que veut dire aller au bout de
			// l'archive, et une troncature se voit ici.
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return membres, tmpDB, fmt.Errorf("archive illisible (tronquée ?) : %w", err)
			}
			continue
		}
		tmpDB = path + ".verify.db"
		if err := removeDB(tmpDB); err != nil {
			return membres, "", err
		}
		out, err := os.OpenFile(tmpDB, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return membres, "", err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return membres, tmpDB, fmt.Errorf("extraction de %s : %w", c.Lot.Base, copyErr)
		}
		if closeErr != nil {
			return membres, tmpDB, closeErr
		}
	}
	return membres, tmpDB, nil
}

// restoreLot déplie un lot descellé dans le dossier dest, qui n'existe pas
// encore : la base, et les fichiers à côté, dans la forme où l'app les attend.
func (c Config) restoreLot(plain []byte, dest string) error {
	// Le lot est vérifié avant d'être étalé : une archive mauvaise ne laisse rien
	// d'écrit à moitié.
	tmp := dest + ".tar"
	if err := CheckTarget(tmp); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, plain, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := c.Verify(tmp); err != nil {
		return fmt.Errorf("sauvegarde refusée : %w", err)
	}
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}
	return deplier(tmp, dest)
}

// deplier extrait un lot vérifié dans un dossier existant.
func deplier(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// L'archive a été écrite par ce paquet et vient d'être vérifiée, mais un
		// extracteur qui fait confiance au nom d'un membre est la façon dont un tar
		// sort de son dossier. Tout ce qui ne reste pas dedans est refusé.
		cible, err := chemin(dest, h.Name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(cible), 0o750); err != nil {
			return err
		}
		out, err := os.OpenFile(cible, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		if closeErr := out.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return copyErr
		}
	}
}

// chemin résout un nom de membre dans dest, et refuse celui qui en sort.
func chemin(dest, membre string) (string, error) {
	propre := filepath.Clean(filepath.FromSlash(membre))
	if filepath.IsAbs(propre) || propre == ".." || strings.HasPrefix(propre, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("nom de membre refusé : %q", membre)
	}
	return filepath.Join(dest, propre), nil
}

// Colonne fabrique une fonction References à partir de colonnes qui nomment
// chacune un fichier, rangé dans le lot sous un préfixe — la forme la plus
// courante. Une table qui n'existe pas encore dans la copie est sautée : un lot
// pris avant qu'elle existe reste un lot valide de son époque.
func Colonne(refs ...ColonneRef) func(db *sql.DB) ([]Reference, error) {
	return func(db *sql.DB) ([]Reference, error) {
		var out []Reference
		for _, r := range refs {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, r.Table).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				continue
			}
			// La requête est dérivée de la table et de la colonne, pas écrite à
			// côté : une colonne et une requête qui ne s'accordent pas vérifieraient
			// une colonne en croyant en vérifier une autre.
			q := fmt.Sprintf(`SELECT %q FROM %q WHERE %q <> ''`, r.Colonne, r.Table, r.Colonne)
			rows, err := db.Query(q)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					_ = rows.Close()
					return nil, err
				}
				out = append(out, Reference{Membre: r.Membre + filepath.ToSlash(v), Table: r.Table})
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// ColonneRef dit qu'une colonne d'une table nomme un fichier rangé sous Membre.
type ColonneRef struct {
	Membre  string // préfixe dans l'archive, par exemple « documents/ »
	Table   string
	Colonne string
}
