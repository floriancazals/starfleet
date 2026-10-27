package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copyMarker est le fichier témoin qui dit qu'un dossier de copie est bien le
// disque attendu. Le point de montage d'un disque non monté est un dossier vide
// ordinaire, sur la carte SD : écrire à l'aveugle y empilerait les copies sur le
// disque même auquel elles doivent survivre. Le témoin se pose une fois, à la
// main, sur le disque monté.
const copyMarker = ".backup-target"

// pushMarker note, dans le dossier des copies locales, la dernière fois qu'une
// copie a QUITTÉ la machine. C'est sur elle que se mesure l'âge quand une
// destination hors machine est configurée.
const pushMarker = ".last-push"

// CopyOut scelle la copie snap, l'écrit dans r.CopyDir, la relit pour la
// vérifier, puis fait le ménage du dossier de copie selon sa propre rétention.
func (r *Runner) CopyOut(snap Snapshot, now time.Time) error {
	if r.CopyDir == "" {
		return errors.New("aucun dossier de copie configuré")
	}
	if len(r.Key) == 0 {
		return errors.New("BACKUP_KEY est vide : une copie qui quitte la machine est scellée, ou elle n'est pas écrite")
	}
	if err := CheckCopyDir(r.CopyDir); err != nil {
		return err
	}
	dst := filepath.Join(r.CopyDir, SealedName(snap.Path))
	if err := r.Cfg.SealFile(r.Key, snap.Path, dst); err != nil {
		return err
	}
	if err := r.Cfg.VerifySealed(r.Key, dst, snap.Size); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("copie refusée : %w", err)
	}
	if removed, err := r.Cfg.PruneSealed(r.CopyDir, r.CopyKeep); err != nil {
		r.log().Error("copie écrite, le ménage du dossier de copie a échoué", "error", err)
	} else if removed > 0 {
		r.log().Info("anciennes copies supprimées", "count", removed, "dir", r.CopyDir)
	}
	if err := markPushed(r.Cfg.Dir, now); err != nil {
		return fmt.Errorf("copie écrite, mais la noter a échoué : %w", err)
	}
	return nil
}

// CheckCopyDir refuse un dossier de copie qui ne porte pas le témoin.
func CheckCopyDir(dir string) error {
	marker := filepath.Join(dir, copyMarker)
	if _, err := os.Stat(marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s ne porte pas de %s : le disque n'est probablement pas monté, et écrire là poserait les copies sur le disque même auquel elles doivent survivre (le créer avec : touch %s)",
				dir, copyMarker, marker)
		}
		return err
	}
	return nil
}

func markPushed(dir string, t time.Time) error {
	return os.WriteFile(filepath.Join(dir, pushMarker), []byte(t.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// LastPush dit quand une copie a quitté la machine pour la dernière fois.
func LastPush(dir string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(dir, pushMarker))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
