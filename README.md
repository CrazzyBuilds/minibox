# minibox

Un mini-runtime de conteneurs en Go (~450 lignes, sans dépendance), écrit pour
comprendre ce que fait vraiment Docker : un conteneur n'est qu'un processus
Linux, avec des **namespaces** (ce qu'il voit), des **cgroups** (ce qu'il peut
consommer) et un **système de fichiers** (l'image).

Tout fonctionne **sans root** (mode *rootless*, comme Podman).

```
$ ./minibox run -image images/sysinfo -memory 256M -cpus 0.5 -p 8123:8000
Server listening on http://localhost:8000

$ curl localhost:8123/api/v1/sysinfo     # depuis l'hôte
```

## Étapes

Chaque étape correspond à un commit, et est expliquée (schémas, expériences,
questions) dans [EXPLICATIONS.md](EXPLICATIONS.md).

| Étape | Mécanisme |
|---|---|
| 1 | namespaces **user** (root sans privilèges) et **UTS** (hostname) |
| 2 | namespaces **PID** et **mount**, `/proc` neuf |
| 3 | `pivot_root` dans le système de fichiers d'une image |
| 4 | **overlayfs** sur les couches de l'image, une couche inscriptible par conteneur |
| 5 | **cgroups v2** : mémoire, CPU, nombre de processus |
| 6 | namespace **réseau**, connexion et publication de ports avec `pasta` |
| 7 | namespace **cgroup** et faux `/proc/meminfo` (façon lxcfs) |

## Utilisation

Prérequis : Linux avec cgroups v2 et user namespaces, Go, Docker (seulement
pour extraire les images), `passt` (pour le réseau).

```sh
go build -o minibox .

# Extraire une image Docker, couche par couche
./pull-image.sh crazzybuilds/sysinfo-api:0.0.2 images/sysinfo

# Lancer son CMD, comme docker run
./minibox run -image images/sysinfo

# Un shell dans le conteneur
./minibox run -image images/sysinfo /bin/sh
```

| Option | Rôle |
|---|---|
| `-image DIR` | image extraite par `pull-image.sh` (couches + configuration) |
| `-rootfs DIR` | dossier utilisé tel quel comme racine (voir `export-rootfs.sh`) |
| `-workdir DIR` | dossier courant dans le conteneur |
| `-memory 256M` | mémoire maximale (sans swap) |
| `-cpus 0.5` | quota de CPU |
| `-pids 64` | nombre maximal de processus |
| `-fake-meminfo` | `/proc/meminfo` annonce la limite mémoire |
| `-net none\|host\|pasta` | réseau isolé (défaut), celui de l'hôte, ou connecté via pasta |
| `-p 8123:8000` | publie un port (hôte:conteneur), implique `-net pasta` |

## Limites connues

Projet pédagogique, pas un runtime de production :

- un seul utilisateur mappé : la commande tourne en root dans le conteneur ;
- les suppressions de fichiers entre couches (*whiteouts* OCI) ne sont pas
  gérées par `pull-image.sh` ;
- le faux `/proc/meminfo` est figé au démarrage (lxcfs le régénère via FUSE) ;
- pas de `--init` : la commande est PID 1 et ignore `SIGTERM` sans
  gestionnaire ;
- pas de seccomp, de capabilities ni d'AppArmor.

Le dossier `containers/` garde la couche inscriptible des conteneurs arrêtés,
pour pouvoir l'inspecter.
