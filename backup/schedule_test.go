package backup

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeNotifier struct {
	msgs    []string
	sendErr error
}

func (f *fakeNotifier) Send(_ context.Context, text string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.msgs = append(f.msgs, text)
	return nil
}

func (f *fakeNotifier) texts() []string { return f.msgs }

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newRunner(t *testing.T, dir string, db Snapshotter, n *fakeNotifier) *Runner {
	t.Helper()
	r := &Runner{
		DB:     db,
		Cfg:    cfgIn(dir, 7),
		Loc:    time.UTC,
		At:     4*60 + 50,
		MaxAge: 48 * time.Hour,
		Log:    quietLog(),
	}
	if n != nil {
		r.Notify = n
	}
	return r
}

func at(d int, hour, minute int) time.Time {
	return time.Date(2026, 8, 20+d, hour, minute, 0, 0, time.UTC)
}

// One snapshot a day, taken once the hour has come — and a restart in the middle
// of the day does not take another one. That last part is what keeps a
// crash-looping container from pushing the real history out of the retention
// window within minutes.
func TestOneSnapshotADayWhateverTheRestarts(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir, newLive(t, 2), &fakeNotifier{})
	ctx := context.Background()

	r.tick(ctx, at(0, 4, 44))
	if all, _ := cfgIn(dir, 0).List(); len(all) != 0 {
		t.Fatalf("before the hour: %v, want nothing", names(all))
	}
	r.tick(ctx, at(0, 4, 50))
	r.tick(ctx, at(0, 4, 46))
	r.tick(ctx, at(0, 23, 59))
	if all, _ := cfgIn(dir, 0).List(); len(all) != 1 {
		t.Fatalf("day 1: %v, want a single snapshot", names(all))
	}

	// A fresh process, same directory: the decision is read from the disk, not
	// from a field, so it still knows today is done.
	fresh := newRunner(t, dir, newLive(t, 2), &fakeNotifier{})
	fresh.tick(ctx, at(0, 20, 0))
	if all, _ := cfgIn(dir, 0).List(); len(all) != 1 {
		t.Fatalf("after a restart the same day: %v, want a single snapshot", names(all))
	}
	fresh.tick(ctx, at(1, 4, 50))
	if all, _ := cfgIn(dir, 0).List(); len(all) != 2 {
		t.Fatalf("next day: %v, want two snapshots", names(all))
	}
}

// L'alerte ne porte que du texte : aucun câblage ne peut faire partir la base
// par le canal des alertes.
func TestLAlerteNePorteQueDuTexte(t *testing.T) {
	iface := reflect.TypeOf((*Alerter)(nil)).Elem()
	if iface.NumMethod() != 1 || iface.Method(0).Name != "Send" {
		t.Fatalf("Alerter a %d méthodes (%v) : il en faut une seule, Send(ctx, text)", iface.NumMethod(), iface)
	}
	send := iface.Method(0).Type
	for i := 0; i < send.NumIn(); i++ {
		if in := send.In(i); in.Kind() == reflect.Slice && in.Elem().Kind() == reflect.Uint8 {
			t.Errorf("Send prend un []byte (argument %d) : c'est un fichier, et un fichier ne part pas par là", i)
		}
	}
}

// Le paquet n'a aucun chemin réseau : il n'importe rien qui parle au réseau, et
// le Runner n'offre à l'app aucun champ par lequel brancher un envoi. Le seul
// branchement passe par backup/expedition, via un paquet internal qu'aucune app
// ne peut importer. Les commentaires sont ôtés avant la lecture : la note du
// paquet doit pouvoir *dire* qu'il n'y a pas de réseau ici.
func TestAucunCheminReseauDansCePaquet(t *testing.T) {
	interdits := []string{`"net"`, `"net/http"`, `"net/url"`, `"mime/multipart"`, `/notify"`, `/expedition"`}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		nm := e.Name()
		if e.IsDir() || !strings.HasSuffix(nm, ".go") || strings.HasSuffix(nm, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, nm, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s : %v", nm, err)
		}
		for _, imp := range f.Imports {
			for _, bad := range interdits {
				if strings.HasSuffix(imp.Path.Value, bad) || imp.Path.Value == bad {
					t.Errorf("%s importe %s : ce paquet ne doit avoir aucun chemin réseau", nm, imp.Path.Value)
				}
			}
		}
	}
	rt := reflect.TypeOf(Runner{})
	for i := 0; i < rt.NumField(); i++ {
		fld := rt.Field(i)
		if !fld.IsExported() {
			continue
		}
		switch fld.Type.Kind() {
		case reflect.Func:
			t.Errorf("Runner.%s est une fonction exportée : une app pourrait y brancher un envoi", fld.Name)
		case reflect.Interface:
			for j := 0; j < fld.Type.NumMethod(); j++ {
				m := fld.Type.Method(j)
				for k := 0; k < m.Type.NumIn(); k++ {
					if in := m.Type.In(k); in.Kind() == reflect.Slice && in.Elem().Kind() == reflect.Uint8 {
						t.Errorf("Runner.%s.%s prend un []byte : un fichier pourrait partir par là", fld.Name, m.Name)
					}
				}
			}
		}
	}
	if (&Runner{Key: []byte("k")}).Ships() {
		t.Error("un Runner neuf ne doit rien envoyer : seul backup/expedition branche un envoi")
	}
}

// The alert is on the age of the last snapshot, never on the success of one.
func TestTheAlertIsAboutSilence(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	r := newRunner(t, dir, newLive(t, 2), n)
	ctx := context.Background()

	// A successful day says nothing at all.
	r.tick(ctx, at(0, 4, 50))
	if len(n.msgs) != 0 {
		t.Fatalf("a successful backup must be silent, sent: %q", n.texts())
	}

	// One day old is fine.
	r.checkAge(ctx, at(1, 5, 0))
	if len(n.msgs) != 0 {
		t.Fatalf("a one-day-old snapshot is fine, sent: %q", n.texts())
	}

	// Three days later it speaks — and says what it actually knows: the local
	// snapshot, which is all this app protects on its own.
	r.checkAge(ctx, at(3, 5, 0))
	if len(n.msgs) != 1 {
		t.Fatalf("sent %d alerts, want 1", len(n.msgs))
	}
	if want := "⚠️ Sauvegarde\nAucune sauvegarde locale depuis 3 jours."; n.msgs[0] != want {
		t.Errorf("alert = %q, want %q", n.msgs[0], want)
	}
	// And it says it once a day, not every hour.
	r.checkAge(ctx, at(3, 6, 0))
	r.checkAge(ctx, at(3, 7, 0))
	if len(n.msgs) != 1 {
		t.Errorf("sent %d alerts the same day, want 1", len(n.msgs))
	}
	r.checkAge(ctx, at(4, 6, 0))
	if len(n.msgs) != 2 {
		t.Errorf("sent %d alerts, want a new one the next day", len(n.msgs))
	}
}

// Whatever the alert says, it names nobody: it travels on a chat channel, and
// this database holds the first names and birth years of other people's
// children.
func TestTheAlertNamesNobody(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	ctx := context.Background()

	// One that reports silence, one that reports a failed backup: the two
	// sentences this package can ever produce.
	ok := newRunner(t, dir, newLive(t, 3), n)
	ok.tick(ctx, at(0, 4, 50))
	ok.checkAge(ctx, at(3, 5, 0))
	broken := newRunner(t, t.TempDir(), garbage{}, n)
	broken.tick(ctx, at(0, 4, 50))

	if len(n.msgs) != 2 {
		t.Fatalf("alerts = %q, want the two the package can produce", n.texts())
	}
	for _, m := range n.msgs {
		if strings.Contains(m, nomFournisseur) {
			t.Errorf("alert %q names a person", m)
		}
		if !strings.HasPrefix(m, "⚠️ Sauvegarde\n") {
			t.Errorf("alert %q is not one of the fixed sentences", m)
		}
	}
}

// A fresh install has no backup at all, and must stay quiet until it has had its
// chance — then say so.
func TestAFreshInstallIsQuietThenSpeaks(t *testing.T) {
	n := &fakeNotifier{}
	r := newRunner(t, t.TempDir(), newLive(t, 1), n)
	r.started = at(0, 3, 0)

	r.checkAge(context.Background(), at(0, 4, 0))
	if len(n.msgs) != 0 {
		t.Fatalf("an hour after boot with no backup yet: %q, want silence", n.texts())
	}
	r.checkAge(context.Background(), at(3, 4, 0))
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "depuis le démarrage") {
		t.Errorf("alerts = %q, want one saying nothing has been backed up since boot", n.texts())
	}
}

// Le cas « rien n'a jamais été sauvegardé » a sa propre phrase. Le libellé de
// repli porte déjà son « depuis », et coller la durée derrière lui écrivait
// « aucune sauvegarde depuis le démarrage du service depuis 3 jours ».
func TestLAlerteDuJamaisSauvegardeNePorteQuUnSeulDepuis(t *testing.T) {
	n := &fakeNotifier{}
	r := newRunner(t, t.TempDir(), newLive(t, 1), n)
	r.started = at(0, 0, 0)

	r.checkAge(context.Background(), at(3, 9, 0))
	want := "⚠️ Sauvegarde\nAucune sauvegarde depuis le démarrage du service, il y a 3 jours."
	if len(n.msgs) != 1 || n.msgs[0] != want {
		t.Fatalf("alertes = %q, want %q", n.texts(), want)
	}
	if strings.Contains(n.msgs[0], "service depuis") {
		t.Errorf("la phrase porte deux « depuis » : %q", n.msgs[0])
	}
}

// A failed backup is not silence: it is said on the spot, and the previous
// snapshot is untouched.
func TestAFailedBackupIsSaidImmediately(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	good, err := cfgIn(dir, 7).Take(context.Background(), newLive(t, 2), day(0))
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, dir, garbage{}, n)

	r.tick(context.Background(), at(1, 4, 50))
	if len(n.msgs) != 1 || !strings.Contains(n.msgs[0], "échoué") {
		t.Fatalf("alerts = %q, want one saying the backup failed", n.texts())
	}
	all, _ := cfgIn(dir, 0).List()
	if len(all) != 1 || all[0].Path != good.Path {
		t.Errorf("snapshots = %v, want the last good one untouched", names(all))
	}
}

// counting wraps a Snapshotter to count the attempts.
type counting struct {
	inner Snapshotter
	calls int
}

func (c *counting) SnapshotInto(ctx context.Context, path string) error {
	c.calls++
	return c.inner.SnapshotInto(ctx, path)
}

// A backup that fails is retried, but not every minute: a read-only volume would
// otherwise write some 1400 identical error lines a day, which is how a real
// error becomes invisible.
func TestRetriesAreSpacedOutAfterAFailure(t *testing.T) {
	broken := &counting{inner: garbage{}}
	n := &fakeNotifier{}
	r := newRunner(t, t.TempDir(), broken, n)
	ctx := context.Background()

	r.tick(ctx, at(0, 4, 50))
	r.tick(ctx, at(0, 4, 46))
	r.tick(ctx, at(0, 5, 30))
	if broken.calls != 1 {
		t.Errorf("attempted %d times inside the hour, want 1", broken.calls)
	}
	r.tick(ctx, at(0, 5, 50)) // past retryAfter
	if broken.calls != 2 {
		t.Errorf("attempted %d times after an hour, want a retry", broken.calls)
	}
	// And it still only says it once a day, however many attempts failed.
	if len(n.msgs) != 1 {
		t.Errorf("sent %q, want 1 alert", n.texts())
	}
}

// Retention runs after the snapshot and never raises an alarm: the copy of the
// day is on disk, which is what was asked for.
func TestRetentionIsQuiet(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{}
	r := newRunner(t, dir, newLive(t, 2), n)
	r.Cfg = cfgIn(dir, 2)

	for d := 0; d < 4; d++ {
		r.tick(context.Background(), at(d, 4, 50))
	}
	kept, _ := cfgIn(dir, 0).List()
	if len(kept) != 2 {
		t.Errorf("kept %v, want the 2 most recent", names(kept))
	}
	if len(n.msgs) != 0 {
		t.Errorf("sent %q, want silence: nothing failed", n.texts())
	}
}

// The age of the last backup is looked at once a day, at a human hour. Hourly
// bought nothing — the alert is capped at one a day anyway — except the chance of
// ringing at 05:00, which is how a channel gets muted.
func TestTheAgeIsLookedAtOnceADayAtAHumanHour(t *testing.T) {
	n := &fakeNotifier{}
	r := newRunner(t, t.TempDir(), newLive(t, 1), n)
	r.At = 23 * 60 // out of the way: this test is about the age check, not the snapshot
	r.started = at(0, 0, 0)
	ctx := context.Background()

	r.tick(ctx, at(9, 8, 0))
	if len(n.msgs) != 0 {
		t.Fatalf("alerts before %02d:00 = %q, want none", alertAt, n.texts())
	}
	r.tick(ctx, at(9, 9, 0))
	if len(n.msgs) != 1 {
		t.Fatalf("alerts at %02d:00 = %q, want one", alertAt, n.texts())
	}
	r.tick(ctx, at(9, 10, 0))
	r.tick(ctx, at(9, 22, 0)) // still before r.At: no snapshot to reset the age
	if len(n.msgs) != 1 {
		t.Errorf("alerts later the same day = %q, want still one", n.texts())
	}
	r.tick(ctx, at(10, 9, 0))
	if len(n.msgs) != 2 {
		t.Errorf("alerts = %q, want a new one the next day", n.texts())
	}
}

// An age past the limit must leave a trace even with nothing to notify through:
// the log line is free, and the journal is where you look afterwards.
func TestAnOldBackupIsLoggedEvenWithoutANotifier(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir, newLive(t, 1), nil)
	r.Notify = nil
	var logged strings.Builder
	r.Log = slog.New(slog.NewTextHandler(&logged, nil))

	if _, err := cfgIn(dir, 7).Take(t.Context(), newLive(t, 1), at(0, 4, 50)); err != nil {
		t.Fatal(err)
	}
	r.checkAge(context.Background(), at(5, 9, 0))
	if !strings.Contains(logged.String(), "sauvegarde trop ancienne") {
		t.Errorf("nothing was logged about a five-day-old backup: %q", logged.String())
	}
}

// Snapshots do not depend on the alert channel: an app with no Telegram still
// protects itself against a bad write, it just cannot complain about silence.
func TestWithoutAChannelSnapshotsStillHappen(t *testing.T) {
	dir := t.TempDir()
	r := newRunner(t, dir, newLive(t, 2), nil)
	if r.Alerts() {
		t.Error("Alerts() must be false without a notifier")
	}
	r.tick(context.Background(), at(0, 4, 50))
	if all, _ := cfgIn(dir, 0).List(); len(all) != 1 {
		t.Fatalf("snapshots = %v, want one — they do not need a channel", names(all))
	}
	r.checkAge(context.Background(), at(9, 5, 0)) // must not panic on a nil notifier
}

// An alert that could not be delivered is not marked as sent, so the next hour
// tries again — the silence must not be swallowed by a broken channel.
func TestAnUndeliveredAlertIsRetried(t *testing.T) {
	dir := t.TempDir()
	n := &fakeNotifier{sendErr: os.ErrDeadlineExceeded}
	r := newRunner(t, dir, newLive(t, 1), n)
	r.started = at(0, 0, 0)

	r.checkAge(context.Background(), at(3, 5, 0))
	n.sendErr = nil
	r.checkAge(context.Background(), at(3, 6, 0))
	if len(n.msgs) != 1 {
		t.Errorf("alerts = %q, want the retry to deliver one", n.texts())
	}
}
