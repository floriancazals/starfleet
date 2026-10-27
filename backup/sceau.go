package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxSealBytes borne ce que Seal et Unseal tiennent en mémoire. Une copie est
// scellée d'un bloc — simple, et juste de façon évidente —, ce qui convient tant
// que la base fait quelques mégaoctets : un stock, des commandes, un annuaire
// sont du texte. Le jour où une copie dépasse, la réponse est un format en flux
// découpé (celui des vidéos de mariage), pas un tampon plus gros.
const maxSealBytes = 256 << 20

// NewKey rend une clé de sauvegarde neuve, en hexadécimal — ce qu'imprime
// -genkey et ce qu'attend BACKUP_KEY. La flotte n'en a qu'une : reprendre celle
// d'une voisine plutôt que d'en tirer une nouvelle.
func NewKey() (string, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return hex.EncodeToString(k), nil
}

// ParseKey décode une clé de sauvegarde, en hexadécimal ou en base64 standard.
// Elle refuse tout ce qui ne fait pas exactement KeySize octets : une clé courte
// est une faute de frappe ou une copie tronquée, et l'étirer ne ferait que le
// cacher jusqu'au jour de la restauration.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("BACKUP_KEY est vide")
	}
	if k, err := hex.DecodeString(s); err == nil && len(k) == KeySize {
		return k, nil
	}
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == KeySize {
		return k, nil
	}
	return nil, fmt.Errorf("une clé de sauvegarde fait %d octets, en hexadécimal ou en base64 (voir -genkey)", KeySize)
}

// Seal compresse puis chiffre une copie : gzip, puis AES-256-GCM, derrière
// l'en-tête de l'app. Rien ne quitte la machine sans être scellé.
func (c Config) Seal(key, plain []byte) ([]byte, error) {
	if err := c.checkMagic(); err != nil {
		return nil, err
	}
	block, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(plain) > maxSealBytes {
		return nil, fmt.Errorf("la copie fait %d octets, au-delà de la limite de %d octets de ce format en un bloc", len(plain), maxSealBytes)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	nonce := make([]byte, block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(c.Magic)+len(nonce)+zipped.Len()+block.Overhead())
	out = append(out, c.Magic...)
	out = append(out, nonce...)
	return block.Seal(out, nonce, zipped.Bytes(), []byte(c.Magic)), nil
}

// Unseal défait Seal. Une mauvaise clé, un téléchargement tronqué et un octet
// modifié sont ici la même erreur : GCM authentifie avant de déchiffrer quoi que
// ce soit.
func (c Config) Unseal(key, blob []byte) ([]byte, error) {
	if err := c.checkMagic(); err != nil {
		return nil, err
	}
	block, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	head := len(c.Magic) + block.NonceSize()
	if !bytes.HasPrefix(blob, []byte(c.Magic)) {
		for ancien, explication := range c.Anciens {
			if bytes.HasPrefix(blob, []byte(ancien)) {
				return nil, errors.New(explication)
			}
		}
		if len(blob) < head+block.Overhead() {
			return nil, errors.New("ce n'est pas une copie scellée : trop courte")
		}
		return nil, errors.New("ce n'est pas une copie scellée de cette app : en-tête inconnu")
	}
	if len(blob) < head+block.Overhead() {
		return nil, errors.New("ce n'est pas une copie scellée : trop courte")
	}
	nonce := blob[len(c.Magic):head]
	zipped, err := block.Open(nil, nonce, blob[head:], []byte(c.Magic))
	if err != nil {
		return nil, fmt.Errorf("copie scellée refusée (mauvaise clé, ou fichier modifié) : %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	plain, err := io.ReadAll(io.LimitReader(zr, maxSealBytes+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > maxSealBytes {
		return nil, errors.New("la copie scellée se déplie au-delà de la limite de ce format")
	}
	return plain, nil
}

// SealFile scelle la copie src dans dst, qui ne doit pas exister : -export qui
// refuse un fichier existant fait écho à -restore, et pour la même raison — le
// chemin qu'on tape de travers est celui qui contenait déjà quelque chose.
func (c Config) SealFile(key []byte, src, dst string) error {
	if err := CheckTarget(dst); err != nil {
		return err
	}
	plain, err := readAtMost(src)
	if err != nil {
		return err
	}
	blob, err := c.Seal(key, plain)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, blob, 0o600)
}

// Restore descelle la copie src et l'écrit à dest, qui ne doit pas exister — une
// restauration ratée ne peut donc pas écraser une base vivante. Pour la base
// seule, dest est le fichier de la base ; pour un lot, c'est un dossier où la
// base et ses fichiers sont dépliés. Une copie qui ne passe pas la vérification
// est effacée plutôt que laissée avec l'air d'être restaurable.
func (c Config) Restore(key []byte, src, dest string) error {
	if err := CheckTarget(dest); err != nil {
		return err
	}
	blob, err := readAtMost(src)
	if err != nil {
		return err
	}
	plain, err := c.Unseal(key, blob)
	if err != nil {
		return err
	}
	if c.Lot != nil {
		return c.restoreLot(plain, dest)
	}
	if err := os.WriteFile(dest, plain, 0o600); err != nil {
		return err
	}
	if err := verifyDB(dest); err != nil {
		_ = removeDB(dest)
		return fmt.Errorf("copie restaurée refusée : %w", err)
	}
	return nil
}

// Restored dit où Restore a posé la base d'un lot déplié dans dest, et où elle
// a posé chacun de ses dossiers (clé : le préfixe du membre, « documents/ »).
// Pour la base seule, dest est la base, et il n'y a aucun dossier.
func (c Config) Restored(dest string) (db string, dossiers map[string]string) {
	if c.Lot == nil {
		return dest, nil
	}
	dossiers = make(map[string]string, len(c.Lot.Dossiers))
	for _, d := range c.Lot.Dossiers {
		dossiers[d.Membre] = filepath.Join(dest, filepath.FromSlash(strings.TrimSuffix(d.Membre, "/")))
	}
	return filepath.Join(dest, filepath.FromSlash(c.Lot.Base)), dossiers
}

// VerifySealed relit une copie scellée et l'ouvre, en vérifiant qu'elle rend
// wantSize octets. C'est ce qui fait d'une copie sur un disque externe une
// sauvegarde plutôt qu'un espoir : un disque qui ment, un câble qui perd des
// octets, une mauvaise clé — les trois ont l'air d'un très bon fichier jusqu'à ce
// que quelqu'un essaie de l'ouvrir, et le seul bon moment pour essayer, c'est
// maintenant.
func (c Config) VerifySealed(key []byte, path string, wantSize int64) error {
	blob, err := readAtMost(path)
	if err != nil {
		return err
	}
	plain, err := c.Unseal(key, blob)
	if err != nil {
		return err
	}
	if int64(len(plain)) != wantSize {
		return fmt.Errorf("la copie scellée porte %d octets, on en attendait %d", len(plain), wantSize)
	}
	return nil
}

// SealedName est le nom de fichier de la forme scellée d'une copie.
func SealedName(snapshotPath string) string {
	return filepath.Base(snapshotPath) + sealedExt
}

// CheckTarget refuse une cible qui existe déjà. -export et -restore écrivent des
// fichiers que l'opérateur nomme à la main, parfois à deux heures du matin,
// par-dessus une base qui est peut-être la vivante. Elle est exportée pour qu'une
// commande vérifie la cible AVANT la partie coûteuse : découvrir que le fichier
// existe après avoir pris la copie gâche une place de rétention pour rien.
func CheckTarget(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return fmt.Errorf("%s existe déjà : écrire dans un fichier neuf, puis le déplacer", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("une clé de sauvegarde fait %d octets, celle-ci en fait %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// readAtMost lit un fichier, et refuse celui qui dépasse ce que ce format tient.
func readAtMost(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSealBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSealBytes {
		return nil, fmt.Errorf("%s dépasse la limite de %d octets de ce format en un bloc", filepath.Base(path), maxSealBytes)
	}
	return b, nil
}
