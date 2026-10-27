package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	hexKey, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseKey(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSealRoundTrip(t *testing.T) {
	key := testKey(t)
	plain := bytes.Repeat([]byte("SQLite format 3\x00 pretend pages "), 500)

	blob, err := tc.Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point: what leaves the machine does not contain what was in the
	// database. gzip makes the blob smaller, but that is not the property tested
	// here — the absence of the plaintext is.
	if bytes.Contains(blob, []byte("pretend pages")) {
		t.Fatal("the sealed blob still contains the database bytes")
	}
	back, err := tc.Unseal(key, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, plain) {
		t.Error("unsealed bytes differ from the original")
	}
}

func TestUnsealRejectsAWrongKeyAndATamperedBlob(t *testing.T) {
	key, other := testKey(t), testKey(t)
	blob, err := tc.Seal(key, []byte("a database, honest"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Unseal(other, blob); err == nil {
		t.Error("another key must not open the blob")
	}

	flipped := bytes.Clone(blob)
	flipped[len(flipped)-1] ^= 0x01
	if _, err := tc.Unseal(key, flipped); err == nil {
		t.Error("a flipped byte must be caught (GCM authenticates before decrypting)")
	}

	truncated := blob[:len(blob)-5]
	if _, err := tc.Unseal(key, truncated); err == nil {
		t.Error("a truncated blob must be caught — an interrupted download is the likely case")
	}

	if _, err := tc.Unseal(key, []byte("ESCBAK0\nnope")); err == nil {
		t.Error("a blob with the wrong header must be refused")
	}
}

func TestParseKeyRefusesAnythingButThirtyTwoBytes(t *testing.T) {
	if _, err := ParseKey("deadbeef"); err == nil {
		t.Error("a short hex key must be refused rather than stretched")
	}
	if _, err := ParseKey(""); err == nil {
		t.Error("an empty key must be refused")
	}
	if _, err := ParseKey("  not a key  "); err == nil {
		t.Error("a passphrase must be refused: -genkey exists so nobody invents one")
	}
	// Both encodings a human might paste out of a password manager.
	if _, err := ParseKey("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="); err != nil {
		t.Errorf("base64 key: %v", err)
	}
	if _, err := ParseKey(strings.Repeat("ab", 32)); err != nil {
		t.Errorf("hex key: %v", err)
	}
}

// -export and -restore both write a file the operator names by hand. Neither may
// write over something that is already there.
func TestSealFileRefusesAnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	snap, err := cfgIn(dir, 7).Take(t.Context(), newLive(t, 2), day(0))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "offsite.enc")
	if err := tc.SealFile(key, snap.Path, out); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.SealFile(key, snap.Path, out); err == nil {
		t.Error("sealing over an existing file must be refused")
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the existing file was overwritten anyway")
	}
}

// A restore has to produce a database that SQLite accepts, and must never be
// able to land on top of the live one.
func TestUnsealFileVerifiesAndRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)

	snap, err := cfgIn(dir, 7).Take(t.Context(), newLive(t, 4), day(0))
	if err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(dir, SealedName(snap.Path))
	if err := tc.SealFile(key, snap.Path, sealed); err != nil {
		t.Fatal(err)
	}

	restored := filepath.Join(dir, "restored.db")
	if err := tc.Restore(key, sealed, restored); err != nil {
		t.Fatal(err)
	}
	if err := tc.Verify(restored); err != nil {
		t.Errorf("the restored copy does not verify: %v", err)
	}

	if err := tc.Restore(key, sealed, restored); err == nil {
		t.Error("restoring over an existing file must be refused: that file could be the live database")
	}

	// A sealed artifact that is not a database must not leave a half-restored
	// file behind looking like one.
	bad := filepath.Join(dir, "bad.enc")
	blob, err := tc.Seal(key, []byte("not a database"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.db")
	if err := tc.Restore(key, bad, out); err == nil {
		t.Error("unsealing something that is not a database must fail")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("the rejected copy was left on disk")
	}
}
