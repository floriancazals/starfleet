package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// drive fakes the external drive: a directory that carries the marker.
func drive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, copyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyRunner(t *testing.T, backupDir, copyDir string, key []byte) *Runner {
	t.Helper()
	r := newRunner(t, backupDir, newLive(t, 3), &fakeNotifier{})
	r.Key = key
	r.CopyDir = copyDir
	r.CopyKeep = 3
	// Le notifieur reste branché, et il ne peut pas faire passer ces essais pour
	// la mauvaise raison : il ne porte que du texte, donc le disque est la seule
	// chose de cette app qui puisse écrire le témoin d'âge. Dans les apps sœurs
	// qui poussent aussi vers un salon, il fallait couper ce canal ici, sinon le
	// témoin apparaissait sans qu'aucune copie ait été écrite.
	return r
}

// What lands on the drive is sealed, opens with the key, and holds the database.
func TestCopyOutWritesASealedCopyThatOpens(t *testing.T) {
	dir, disk, key := t.TempDir(), drive(t), testKey(t)
	r := copyRunner(t, dir, disk, key)

	r.tick(t.Context(), at(0, 4, 50))

	copies, err := tc.ListSealed(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("the drive holds %v, want one sealed copy", names(copies))
	}
	blob, err := os.ReadFile(copies[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	// L'en-tête de tout fichier SQLite, et le nom d'un fournisseur. L'un ou
	// l'autre sur un disque qu'on débranche, qu'on prête ou qu'on revend, et ce
	// sont les achats d'un commerçant qui sont partis en clair.
	if bytes.Contains(blob, []byte("SQLite format 3")) || bytes.Contains(blob, []byte(nomFournisseur)) {
		t.Fatal("the copy on the drive is not sealed")
	}
	plain, err := tc.Unseal(key, blob)
	if err != nil {
		t.Fatalf("the copy on the drive cannot be opened: %v", err)
	}
	out := filepath.Join(t.TempDir(), "restaure.db")
	if err := os.WriteFile(out, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tc.Verify(out); err != nil {
		t.Errorf("the copy does not restore into a valid database: %v", err)
	}
	// A copy that left the machine is what the age alert measures.
	if _, ok := LastPush(dir); !ok {
		t.Error("the copy was not recorded")
	}
}

// The guard that matters: an unmounted drive is an ordinary empty directory on the
// machine's own disk, and writing there would pile the copies onto the very disk
// they exist to survive — while every log line said "copy written".
func TestNothingIsWrittenToADriveThatIsNotMounted(t *testing.T) {
	dir, key := t.TempDir(), testKey(t)
	notMounted := t.TempDir() // exists, empty, no marker: exactly what a failed mount looks like
	r := copyRunner(t, dir, notMounted, key)

	r.tick(t.Context(), at(0, 4, 50))

	entries, err := os.ReadDir(notMounted)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d file(s) to a directory with no %s", len(entries), copyMarker)
	}
	// And the silence is not mistaken for success: nothing left the machine, so the
	// age alert still has nothing to measure.
	if _, ok := LastPush(dir); ok {
		t.Error("a copy that was never written was recorded as one")
	}
	// The local snapshot, on the other hand, is there: the destination failing does
	// not cost the day's backup.
	if snaps, _ := cfgIn(dir, 0).List(); len(snaps) != 1 {
		t.Errorf("local snapshots = %v, want the day's copy", names(snaps))
	}
}

// No key, no copy: a drive gets unplugged, carried, lent, resold.
func TestNothingIsWrittenToADriveWithoutAKey(t *testing.T) {
	disk := drive(t)
	r := copyRunner(t, t.TempDir(), disk, nil)

	if r.Copies() {
		t.Error("Copies() must be false without a key")
	}
	r.tick(t.Context(), at(0, 4, 50))
	if copies, _ := tc.ListSealed(disk); len(copies) != 0 {
		t.Errorf("wrote %v to the drive with no key", names(copies))
	}
	if err := r.CopyOut(Snapshot{Path: "x"}, at(0, 4, 50)); err == nil {
		t.Error("CopyOut must refuse without a key")
	}
}

// The drive has its own, deeper retention.
func TestTheDriveKeepsItsOwnHistory(t *testing.T) {
	dir, disk := t.TempDir(), drive(t)
	r := copyRunner(t, dir, disk, testKey(t))
	r.Cfg = cfgIn(dir, 2) // local: two days
	r.CopyKeep = 4        // drive: four

	for d := 0; d < 6; d++ {
		r.tick(t.Context(), at(d, 4, 50))
	}
	if snaps, _ := cfgIn(dir, 0).List(); len(snaps) != 2 {
		t.Errorf("local = %v, want 2", names(snaps))
	}
	copies, _ := tc.ListSealed(disk)
	if len(copies) != 4 {
		t.Errorf("drive = %v, want 4", names(copies))
	}
	if got := filepath.Base(copies[3].Path); got != "traiteur-20260825T045000.000Z.db.gz.enc" {
		t.Errorf("newest copy = %s, want the sixth day", got)
	}
}

// Once a drive is configured, "the last backup" is the last copy that reached it:
// snapshots piling up on the card that is about to die prove nothing. This is
// also what catches a destination that stays broken — a failed copy is never
// alerted on the spot, on purpose, because an unplugged drive on a Tuesday is not
// news.
func TestWithADriveTheAgeIsMeasuredOnTheCopy(t *testing.T) {
	dir, disk := t.TempDir(), drive(t)
	n := &fakeNotifier{}
	r := copyRunner(t, dir, disk, testKey(t))
	r.Notify = n

	r.tick(t.Context(), at(0, 4, 50)) // a copy reaches the drive
	if _, ok := LastPush(dir); !ok {
		t.Fatal("the first copy was not recorded")
	}

	// The drive is unplugged: its mount point is now an ordinary empty directory.
	if err := os.Remove(filepath.Join(disk, copyMarker)); err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= 3; d++ {
		r.tick(t.Context(), at(d, 4, 50))
	}
	if len(n.msgs) != 0 {
		t.Fatalf("a failed copy alerted on the spot: %q", n.texts())
	}
	if snaps, _ := cfgIn(dir, 0).List(); len(snaps) != 4 {
		t.Fatalf("local snapshots = %v, want one a day: the drive failing costs no backup", names(snaps))
	}

	r.checkAge(t.Context(), at(3, 9, 0))
	want := "⚠️ Sauvegarde\nAucune copie n'a quitté la machine depuis 3 jours."
	if len(n.msgs) != 1 || n.msgs[0] != want {
		t.Errorf("alerts = %q, want %q — the age is measured on the copy, not on the snapshot", n.texts(), want)
	}
}

// A copy is verified by being read back — a disk that lies, a cable that drops
// bytes and a wrong key all look like a good file until someone opens it.
func TestVerifySealedCatchesWhatLooksLikeAGoodFile(t *testing.T) {
	dir, key := t.TempDir(), testKey(t)
	snap, err := cfgIn(dir, 7).Take(t.Context(), newLive(t, 3), at(0, 4, 50))
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, SealedName(snap.Path))
	if err := tc.SealFile(key, snap.Path, sealed); err != nil {
		t.Fatal(err)
	}
	if err := tc.VerifySealed(key, sealed, snap.Size); err != nil {
		t.Fatalf("a good copy must verify: %v", err)
	}
	if err := tc.VerifySealed(testKey(t), sealed, snap.Size); err == nil {
		t.Error("another key must not verify")
	}
	if err := tc.VerifySealed(key, sealed, snap.Size+1); err == nil {
		t.Error("a copy of the wrong size must not verify")
	}

	blob, err := os.ReadFile(sealed)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.gz.enc")
	if err := os.WriteFile(truncated, blob[:len(blob)-3], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tc.VerifySealed(key, truncated, snap.Size); err == nil {
		t.Error("a truncated copy must not verify")
	}
}
