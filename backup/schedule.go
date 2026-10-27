package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/floriancazals/starfleet/internal/passerelle"
)

// backupTick est le pas de la boucle. Une minute suffit : la sauvegarde est
// prévue à une minute près, et un tour qui n'a rien à faire ne coûte qu'une
// lecture de dossier.
const backupTick = time.Minute

// alertAt est l'heure locale à partir de laquelle l'âge de la dernière copie est
// regardé — une heure où quelqu'un lira le message.
const alertAt = 9

// retryAfter espace les tentatives après un échec : une base qui ne se copie pas
// ne se copiera pas mieux dans une minute, et le journal n'a pas à le redire
// soixante fois par heure.
const retryAfter = time.Hour

// Alerter porte l'alerte d'une sauvegarde qui se tait. Il ne porte que du TEXTE,
// et c'est délibéré : aucun câblage ne peut faire partir la base par le canal des
// alertes. *notify.Telegram l'implémente.
type Alerter interface {
	Send(ctx context.Context, text string) error
}

// Runner prend une copie par jour à l'heure dite, la pose sur le disque de copie
// s'il y en a un, et alerte quand la dernière sauvegarde est trop vieille.
type Runner struct {
	DB  Snapshotter
	Cfg Config
	Loc *time.Location // fuseau local, pour l'heure prévue
	At  int            // minutes depuis minuit local
	Key []byte         // nil : copies locales seulement, rien ne quitte la machine

	CopyDir  string // dossier hors de la machine (disque externe) ; vide : aucun
	CopyKeep int    // rétention de ce dossier

	MaxAge time.Duration // un silence plus long que ça mérite une alerte
	Notify Alerter       // nil : aucune alerte, les copies se font quand même
	Log    *slog.Logger

	// envoi pose la copie scellée hors de la machine, dans un salon. Il n'est
	// branché que par le sous-paquet expedition : voir la note du paquet.
	envoi passerelle.Envoi

	started  time.Time // référence de l'alerte d'âge tant que rien n'a été sauvegardé
	alerted  string    // date locale de la dernière alerte, pour n'en envoyer qu'une par jour
	checked  string    // date locale du dernier contrôle d'âge, pour qu'il ait lieu une fois par jour
	lastFail time.Time // quand la dernière tentative a échoué, pour espacer les suivantes
}

func init() {
	passerelle.Expedition = func(runner any, envoi passerelle.Envoi) {
		runner.(*Runner).envoi = envoi
	}
}

// Alerts dit si une sauvegarde qui se tait sera signalée à quelqu'un.
func (r *Runner) Alerts() bool { return r.Notify != nil && r.MaxAge > 0 }

// Copies dit si chaque copie part sur le disque de copie.
func (r *Runner) Copies() bool { return len(r.Key) > 0 && r.CopyDir != "" }

// Ships dit si chaque copie part aussi, scellée, dans un salon.
func (r *Runner) Ships() bool { return len(r.Key) > 0 && r.envoi != nil }

// offMachine dit si une copie est censée quitter la machine chaque jour : l'âge
// se mesure alors sur la dernière copie PARTIE, pas sur la dernière prise.
func (r *Runner) offMachine() bool { return r.Copies() || r.Ships() }

// Run tourne jusqu'à la fin de ctx.
func (r *Runner) Run(ctx context.Context) {
	r.started = time.Now()

	t := time.NewTicker(backupTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx, time.Now().In(r.loc()))
		}
	}
}

func (r *Runner) tick(ctx context.Context, now time.Time) {
	if r.due(now) {
		r.once(ctx, now)
	}
	if day := now.Format("2006-01-02"); r.checked != day && now.Hour() >= alertAt {
		r.checked = day
		r.checkAge(ctx, now)
	}
}

// due dit s'il faut une copie : l'heure est passée et le dossier n'en porte pas
// encore d'aujourd'hui. C'est le disque qui répond, pas la mémoire : un service
// redémarré dix fois dans la journée ne prend qu'une copie.
func (r *Runner) due(now time.Time) bool {
	if now.Hour()*60+now.Minute() < r.At {
		return false
	}
	if !r.lastFail.IsZero() && now.Sub(r.lastFail) < retryAfter {
		return false
	}
	latest, err := r.Cfg.Latest()
	if err != nil {
		return true // pas encore de copie, ou un dossier illisible : essayer, et dire ce qui arrive
	}
	return latest.Taken.In(r.loc()).Format("2006-01-02") != now.Format("2006-01-02")
}

func (r *Runner) once(ctx context.Context, now time.Time) {
	snap, err := r.Cfg.Take(ctx, r.DB, now)
	if err != nil {
		r.lastFail = now
		r.log().Error("la sauvegarde a échoué", "error", err)
		r.alert(ctx, now, "⚠️ Sauvegarde\nLa sauvegarde du jour a échoué : "+err.Error())
		return
	}
	r.lastFail = time.Time{}
	r.log().Info("sauvegarde prise", "path", snap.Path, "bytes", snap.Size)

	defer r.prune()

	// Chaque destination est tentée, même quand l'autre a échoué : un disque
	// débranché ne doit pas priver le salon de sa copie, ni l'inverse. Leurs
	// échecs restent au journal ; l'alerte, elle, vient de l'âge (checkAge).
	if r.Copies() {
		if err := r.CopyOut(snap, now); err != nil {
			r.log().Error("écriture de la copie impossible", "dir", r.CopyDir, "error", err)
		} else {
			r.log().Info("copie écrite", "dir", r.CopyDir, "name", SealedName(snap.Path))
		}
	}
	if r.Ships() {
		if err := r.Ship(ctx, snap, now); err != nil {
			r.log().Error("envoi de la copie impossible", "error", err)
		} else {
			r.log().Info("copie envoyée", "name", SealedName(snap.Path))
		}
	}
}

// Ship scelle la copie et la pose dans le salon branché par expedition.
func (r *Runner) Ship(ctx context.Context, snap Snapshot, now time.Time) error {
	if !r.Ships() {
		return errors.New("rien pour envoyer la copie : BACKUP_KEY est vide, ou aucun salon n'est branché")
	}
	plain, err := readAtMost(snap.Path)
	if err != nil {
		return err
	}
	blob, err := r.Cfg.Seal(r.Key, plain)
	if err != nil {
		return err
	}
	if err := r.envoi(ctx, SealedName(snap.Path), blob); err != nil {
		return err
	}
	if err := markPushed(r.Cfg.Dir, now); err != nil {
		return fmt.Errorf("copie envoyée, mais la noter a échoué : %w", err)
	}
	return nil
}

func (r *Runner) prune() {
	removed, err := r.Cfg.Prune()
	switch {
	case err != nil:
		r.log().Error("sauvegarde conservée, le ménage a échoué", "error", err)
	case removed > 0:
		r.log().Info("anciennes sauvegardes supprimées", "count", removed)
	}
}

// checkAge alerte quand la dernière sauvegarde est trop vieille. L'alerte porte
// sur le SILENCE, jamais sur une réussite : un message par nuit qui dit « tout va
// bien » apprend à ne plus lire, et le jour où il manque, personne ne le remarque.
func (r *Runner) checkAge(ctx context.Context, now time.Time) {
	if r.MaxAge <= 0 {
		return
	}
	last, what := r.lastBackup()
	jamais := last.IsZero()
	if jamais {
		last = r.started
	}
	age := now.Sub(last)
	if age <= r.MaxAge {
		return
	}
	msg := fmt.Sprintf("⚠️ Sauvegarde\n%s depuis %s.", what, humanAge(age))
	if jamais {
		what = "aucune sauvegarde depuis le démarrage du service"
		msg = fmt.Sprintf("⚠️ Sauvegarde\nAucune sauvegarde depuis le démarrage du service, il y a %s.", humanAge(age))
	}
	r.log().Warn("sauvegarde trop ancienne", "age", age.Round(time.Minute).String(), "what", what)
	r.alert(ctx, now, msg)
}

// lastBackup rend l'instant de la dernière sauvegarde qui compte. Avec une
// destination hors machine, c'est la dernière copie PARTIE (.last-push) : une
// copie locale fraîche ne protège pas d'un disque mort.
func (r *Runner) lastBackup() (time.Time, string) {
	if r.offMachine() {
		if t, ok := LastPush(r.Cfg.Dir); ok {
			return t, "Aucune copie n'a quitté la machine"
		}
		return time.Time{}, ""
	}
	snap, err := r.Cfg.Latest()
	if err != nil {
		return time.Time{}, ""
	}
	return snap.Taken, "Aucune sauvegarde locale"
}

// alert envoie une alerte, au plus une par jour. Une alerte non partie n'est pas
// notée : l'heure suivante réessaie.
func (r *Runner) alert(ctx context.Context, now time.Time, text string) {
	if r.Notify == nil {
		return
	}
	day := now.Format("2006-01-02")
	if r.alerted == day {
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := r.Notify.Send(sendCtx, text); err != nil {
		r.log().Error("alerte de sauvegarde non envoyée", "error", err)
		return
	}
	r.alerted = day
}

func (r *Runner) loc() *time.Location {
	if r.Loc == nil {
		return time.UTC
	}
	return r.Loc
}

func (r *Runner) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

// humanAge dit une durée comme on la lit dans un message.
func humanAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d jours", int(d.Hours())/24)
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
}
