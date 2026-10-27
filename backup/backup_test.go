package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveDB stands in for the service's store. The snapshot path is exercised
// against SQLite itself, never a fake: the bug this whole package exists to
// avoid — a copy that opens fine and is missing rows — is a property of SQLite,
// not of our code.
type liveDB struct{ db *sql.DB }

func (l liveDB) SnapshotInto(ctx context.Context, path string) error {
	_, err := l.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// garbage writes something that is not a database at all — a truncated copy, a
// disk that lied, a target on a full filesystem.
type garbage struct{}

func (garbage) SnapshotInto(_ context.Context, path string) error {
	return os.WriteFile(path, []byte("this is not a database"), 0o600)
}

// nomFournisseur est le nom semé dans la base d'essai. Il sert de témoin : le
// retrouver en clair dans un artefact scellé veut dire que les livres de
// l'entreprise sont lisibles par quiconque tient le disque.
const nomFournisseur = "Metro Cash & Carry"

func newLive(t *testing.T, rows int) liveDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE fournisseur (id TEXT PRIMARY KEY, nom TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec(`INSERT INTO fournisseur VALUES (?, ?)`, i, nomFournisseur); err != nil {
			t.Fatal(err)
		}
	}
	return liveDB{db: db}
}

// testMagic est l'en-tête des copies scellées des tests.
const testMagic = "TESTBAK1\n"

// tc est la configuration des tests qui ne dépendent pas du dossier.
var tc = Config{Prefix: "traiteur", Magic: testMagic}

func cfgIn(dir string, keep int) Config {
	return Config{Dir: dir, Prefix: "traiteur", Keep: keep, Magic: testMagic}
}

func day(n int) time.Time {
	return time.Date(2026, 8, 20+n, 4, 50, 0, 0, time.UTC)
}

// A snapshot is a real, readable, complete database — and its name says when it
// was taken.
func TestTakeMakesAReadableCopy(t *testing.T) {
	dir := t.TempDir()
	snap, err := cfgIn(dir, 7).Take(context.Background(), newLive(t, 3), day(0))
	if err != nil {
		t.Fatal(err)
	}
	if base := filepath.Base(snap.Path); base != "traiteur-20260820T045000.000Z.db" {
		t.Errorf("snapshot name = %q, want the UTC stamp", base)
	}
	if snap.Size == 0 {
		t.Error("snapshot size = 0")
	}
	db, err := sql.Open("sqlite", "file:"+snap.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM fournisseur`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("the copy holds %d rows, want 3", n)
	}
	// VACUUM INTO writes one file, with no WAL beside it — that is what makes a
	// snapshot safe to ship as a single artifact.
	for _, sidecar := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(snap.Path + sidecar); err == nil {
			t.Errorf("the snapshot left a %s file behind", sidecar)
		}
	}
}

// The invariant that matters most: a copy that cannot be verified never replaces
// the one that could.
func TestACorruptSnapshotNeverReplacesTheLastGoodOne(t *testing.T) {
	dir := t.TempDir()
	good, err := cfgIn(dir, 7).Take(context.Background(), newLive(t, 2), day(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfgIn(dir, 7).Take(context.Background(), garbage{}, day(1)); err == nil {
		t.Fatal("a snapshot that is not a database must be rejected")
	}
	all, err := cfgIn(dir, 0).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Path != good.Path {
		t.Fatalf("after a failed backup the directory holds %v, want only the good snapshot", names(all))
	}
	// And nothing half-written is left lying around to be shipped or restored.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("a rejected copy was left behind: %s", e.Name())
		}
	}
}

// The file name is dated to the second, so two snapshots in the same second would
// share it — and a silent rename would leave one file behind two successful
// commands. An operator who believes he holds two copies is worse off than one who
// is told to wait.
func TestTwoSnapshotsInTheSameSecondDoNotHideOneAnother(t *testing.T) {
	dir := t.TempDir()
	live := newLive(t, 2)
	first, err := cfgIn(dir, 7).Take(context.Background(), live, day(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfgIn(dir, 7).Take(context.Background(), live, day(0)); err == nil {
		t.Error("a second snapshot for the same second must be refused, not silently merged")
	}
	all, err := cfgIn(dir, 0).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Path != first.Path {
		t.Errorf("directory holds %v, want only the first snapshot", names(all))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("a refused run left %s behind", e.Name())
		}
	}
}

// A run killed halfway leaves a .part file, and VACUUM INTO refuses to write
// over an existing file: without clearing it, every later backup would fail.
func TestALeftoverPartFileDoesNotBlockTheNextBackup(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "traiteur-20260820T045000.000Z.db.part")
	if err := os.WriteFile(part, []byte("half a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgIn(dir, 7).Take(context.Background(), newLive(t, 1), day(0)); err != nil {
		t.Fatalf("a leftover .part must not block the next backup: %v", err)
	}
}

// Retention is a separate step from taking the snapshot, and it drops the oldest
// first. Take never prunes: an error from Take always means "there is no new
// snapshot", so a directory that cannot be tidied is never reported as a failed
// backup.
func TestPruneKeepsTheMostRecent(t *testing.T) {
	dir := t.TempDir()
	live := newLive(t, 1)
	for i := 0; i < 5; i++ {
		if _, err := cfgIn(dir, 3).Take(context.Background(), live, day(i)); err != nil {
			t.Fatal(err)
		}
	}
	if kept, _ := cfgIn(dir, 0).List(); len(kept) != 5 {
		t.Fatalf("Take pruned on its own: %v, want the five snapshots untouched", names(kept))
	}
	if removed, err := cfgIn(dir, 3).Prune(); err != nil || removed != 2 {
		t.Fatalf("Prune removed %d (err %v), want 2", removed, err)
	}
	all, err := cfgIn(dir, 0).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("kept %v, want the 3 most recent", names(all))
	}
	if got := filepath.Base(all[0].Path); got != "traiteur-20260822T045000.000Z.db" {
		t.Errorf("oldest kept = %s, want the 3rd day", got)
	}
	if got := filepath.Base(all[2].Path); got != "traiteur-20260824T045000.000Z.db" {
		t.Errorf("newest kept = %s, want the 5th day", got)
	}
	latest, err := cfgIn(dir, 0).Latest()
	if err != nil || filepath.Base(latest.Path) != "traiteur-20260824T045000.000Z.db" {
		t.Errorf("Latest = %v (%v), want the 5th day", latest.Path, err)
	}
	if !latest.Taken.Equal(day(4)) {
		t.Errorf("Taken = %v, want %v (read from the name, not the mtime)", latest.Taken, day(4))
	}
}

// An empty database passes integrity_check — which is exactly why that check is
// not enough on its own.
func TestVerifyRejectsADatabaseWithoutASchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil { // touch it so the file exists
		t.Fatal(err)
	}
	db.Close()

	if err := cfgIn(t.TempDir(), 0).Verify(path); err == nil {
		t.Error("a database with no table must be rejected: that is what a misdirected copy looks like")
	}
}

func TestLatestOnAnEmptyDirectory(t *testing.T) {
	if _, err := cfgIn(t.TempDir(), 0).Latest(); err != ErrNoSnapshot {
		t.Errorf("err = %v, want ErrNoSnapshot", err)
	}
	if _, err := cfgIn(filepath.Join(t.TempDir(), "nope"), 0).Latest(); err != ErrNoSnapshot {
		t.Errorf("on a missing directory: err = %v, want ErrNoSnapshot", err)
	}
}

func TestPrefixFromPath(t *testing.T) {
	cases := map[string]string{
		"/data/traiteur.db": "traiteur",
		"traiteur.db":       "traiteur",
		"/data/stock":       "stock",
		"/data/a.b.db":      "a.b",
		"":                  "db",
	}
	for in, want := range cases {
		if got := PrefixFromPath(in); got != want {
			t.Errorf("PrefixFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func names(all []Snapshot) []string {
	out := make([]string, 0, len(all))
	for _, s := range all {
		out = append(out, filepath.Base(s.Path))
	}
	return out
}
