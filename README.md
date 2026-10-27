# starfleet

Le code commun de la flotte : ce que chaque app (lease, todo, resa, folio…) copiait
jusqu'ici dans son propre dépôt — audience, backup, gate, notify, en-têtes de
sécurité… Un changement commun se fait ici une fois, au lieu de dix-neuf fois.

## Paquets

| Paquet              | Rôle                                                                  |
|---------------------|-----------------------------------------------------------------------|
| `audience`          | mesure d'audience envoyée par le serveur à l'Umami de la flotte       |
| `gate`              | côté app de l'authentification centrale : vérifie tickets et révocations |
| `notify`            | messages Telegram : `Telegram` (salons de la config, texte), `Bot` (un salon par appel, texte), `Depot` (fichiers) |
| `backup`            | copie quotidienne vérifiée de la base (ou d'un lot base + dossiers), scellée vers le disque de copie, alerte d'âge |
| `backup/expedition` | le SEUL chemin par lequel une base part sur le réseau (un salon Telegram) |
| `antiabus`          | budget de tentatives par adresse (`Limiter`) et la règle de l'adresse client (`ClientIP`, par `X-Real-Ip`) |
| `mail`              | courriels SMTP : `Mailer` (texte, HTML, pièces jointes) et `Courrier` (une pièce, plusieurs destinataires, copie cachée) |

Et pour la CI : `.github/workflows/app.yml`, le workflow que chaque app appelle
depuis son `ci.yml` (vérifier, puis publier l'image). La version de Go, celle du
lint et les étapes se changent là.

### Ce qui ne sort jamais

`backup` n'a aucun chemin réseau, et `notify.Telegram` comme `notify.Bot` ne
portent que du texte. Une base ne part dans un salon que si l'app importe
`backup/expedition` et construit un `notify.Depot` : c'est réservé aux apps dont
les données sont celles de leur auteur (todo, resa, escale-breakfast, lease).
gate, ateliers et les apps des clients ne l'importent jamais.

Chaque app garde son propre en-tête de copie scellée (`backup.Config.Magic`,
« TRAIBAK1\n », « LEASEBAK1\n »…) : il fait partie du format, et le changer
rendrait illisibles les copies déjà écrites.

## Utiliser starfleet dans une app

```sh
go get github.com/floriancazals/starfleet@v0.3.0
```

puis importer `github.com/floriancazals/starfleet/audience`, etc. Le dépôt est
public : la CI, le build Docker et le bac à sable du runner téléchargent le
module par le proxy Go, sans jeton.

## Publier une version

Chaque app épingle une version : une correction ici n'atteint une app qu'au
prochain `go get …@vX.Y.Z` dans cette app, qui la redéploie.

1. `make test lint`, pousser sur `main`, attendre la CI verte.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`. Un tag publié ne se déplace
   **jamais** : le proxy et `sum.golang.org` ont enregistré son contenu, et un
   tag réécrit casse le build de toutes les apps qui l'épinglent.
3. Relever la version dans chaque app (go.mod, et la référence du workflow
   dans son `ci.yml` si `.github/workflows/app.yml` a changé).

## Règles

- Un paquet n'entre ici que s'il est **identique** d'une app à l'autre, ou s'il
  peut le devenir par un réglage : ce qui est propre à une app reste dans l'app.
- Rien de secret ici, le dépôt est public : ni clé, ni jeton, ni adresse
  interne en dur. La configuration arrive de l'app qui importe le paquet.
- Les chaînes visibles sont en français : ne jamais lancer `misspell --fix` sans
  relire le diff (voir `.golangci.yml`).

## Développement

```sh
make test   # go test ./...
make lint   # golangci-lint, même version que la CI
```
