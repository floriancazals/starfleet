# starfleet

Le code commun de la flotte : ce que chaque app (lease, todo, resa, folio…) copiait
jusqu'ici dans son propre dépôt — audience, backup, gate, notify, en-têtes de
sécurité… Un changement commun se fait ici une fois, au lieu de dix-neuf fois.

## État

Dépôt neuf (18/10/2026) : rien n'est encore extrait. La première migration part
de `lease`, l'app la moins critique (tâche todo 237).

## Règles

- Un paquet n'entre ici que s'il est **identique** d'une app à l'autre, ou s'il
  peut le devenir par un réglage : ce qui est propre à une app reste dans l'app.
- Rien de secret ici : ni clé, ni jeton, ni adresse interne en dur. La
  configuration arrive de l'app qui importe le paquet.
- Les chaînes visibles sont en français : ne jamais lancer `misspell --fix` sans
  relire le diff (voir `.golangci.yml`).

## Développement

```sh
make test   # go test ./...
make lint   # golangci-lint, même version que la CI
```
