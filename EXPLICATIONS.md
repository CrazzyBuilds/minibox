# Comprendre minibox

Ce document ré-explique, étape par étape, ce qu'on construit dans minibox et
pourquoi. Chaque étape reprend les **expériences réellement faites** sur la
machine, avec leur résultat.

---

## 0. L'idée de départ : un conteneur n'est qu'un processus

**Docker ne virtualise rien.** Un conteneur est un processus Linux ordinaire,
qui tourne sur le même noyau que tous les autres programmes de la machine.

Preuve : un `docker run alpine sleep 300`, vu depuis l'hôte.

```
$ ps -o pid,ppid,user,cmd -p <pid du conteneur>
    PID    PPID USER     CMD
 147460  147421 root     sleep 300                     ← le "conteneur"
 147421         /usr/bin/containerd-shim-runc-v2 ...    ← son parent
```

Ce qui change, c'est **ce que le processus voit**. Le noyau peut lui montrer une
version différente de certaines ressources : c'est le rôle des **namespaces**.

```
                  ┌─────────────────────────────────────────┐
                  │              NOYAU LINUX                 │
                  │            (un seul, partagé)            │
                  └─────────────────────────────────────────┘
                     ▲               ▲                 ▲
                     │               │                 │
              ┌──────┴─────┐  ┌──────┴─────┐   ┌───────┴──────┐
              │  firefox   │  │   bash     │   │ node (API)   │
              │            │  │            │   │ 👓 namespaces │
              │ vue normale│  │ vue normale│   │ vue à part   │
              └────────────┘  └────────────┘   └──────────────┘
                                                  "le conteneur"
```

> **Un conteneur = un processus + des namespaces (ce qu'il voit)
> + des cgroups (ce qu'il peut consommer) + un système de fichiers (l'image).**

| Ingrédient | Rôle | Étape |
|---|---|---|
| namespace **user** | il se croit root, sans l'être vraiment | 1 |
| namespace **UTS** | son propre hostname | 1 |
| namespace **PID** | ses propres numéros de processus | 2 |
| namespace **mount** | sa propre table de montages | 2 |
| **pivot_root** | les fichiers de l'image deviennent sa racine | 3 |
| **overlayfs** | il écrit sans abîmer l'image | 4 |
| **cgroups** | limite de mémoire et de CPU | 5 |
| namespace **réseau** | sa propre IP, ses propres ports | 6 |
| namespace **cgroup** | son cgroup lui apparaît comme la racine | 7 |

Les namespaces actifs d'un processus sont visibles dans `/proc/<pid>/ns/` :

```
          hôte                    minibox (étape 1)
uts   uts:[4026531838]        uts:[4026534277]        ISOLÉ
user  user:[4026531837]       user:[4026534276]       ISOLÉ
pid   pid:[4026531836]        pid:[4026531836]        partagé
mnt   mnt:[4026531832]        mnt:[4026531832]        partagé
net   net:[4026531833]        net:[4026531833]        partagé
```

Même numéro = même namespace = même vue que l'hôte.

---

## 1. Étape 1 : user et UTS

### Comment minibox crée un conteneur

Les namespaces se créent **au moment où un processus naît** (appel système
`clone()`), grâce à des drapeaux. minibox se relance donc lui-même :

```
  Terminal
     │
     │  ./minibox run /bin/sh
     ▼
┌──────────────────────┐
│ minibox "run"        │  côté hôte, namespaces normaux
│                      │
│ clone() + drapeaux : │
│  CLONE_NEWUSER       │
│  CLONE_NEWUTS        │
└──────────┬───────────┘
           │  crée un enfant DANS de nouveaux namespaces
           ▼
┌──────────────────────┐
│ minibox "child"      │  déjà dans le conteneur
│                      │
│ 1. prépare le dedans │  (hostname, montages…)
│ 2. exec(/bin/sh)     │──┐
└──────────────────────┘  │ exec() remplace le programme :
                          │ même processus, même PID,
           ┌──────────────┘ mêmes namespaces
           ▼
┌──────────────────────┐
│ /bin/sh              │  ta commande, isolée
└──────────────────────┘
```

C'est exactement le rôle de `runc` chez Docker : il prépare le conteneur, puis
**s'efface** au profit de ton `node dist/index.js`.

### Le namespace UTS : un hostname à soi

```
--- hôte
hostname=omarchy
--- conteneur
hostname=minibox
après changement : pirate      ← le conteneur change SON hostname…
--- hôte, après
omarchy                        ← …celui de la machine ne bouge pas
```

C'est pour ça que les conteneurs Docker ont un hostname aléatoire
(`7da031d7e6a0`), et que celui d'Azure s'appelait `SandboxHost-…`.

### Le namespace user : root, mais seulement dans sa bulle

On donne au noyau une **table de correspondance** des utilisateurs :

```
       conteneur              hôte
     ┌───────────┐        ┌───────────┐
     │  uid 0    │ ─────► │ uid 1000  │
     │  (root)   │        │ (nathan)  │
     └───────────┘        └───────────┘
```

- **Dans** le conteneur : `id` affiche `uid=0`, il peut administrer **ses
  propres** namespaces (changer son hostname, monter des choses chez lui).
- **Pour le reste de la machine** : c'est toi, l'utilisateur 1000. Il ne peut
  rien faire de plus que toi.

Preuve que c'est indispensable :

```
$ unshare --uts hostname x        ← sans user namespace
unshare: Operation not permitted  ← il faudrait être vraiment root
```

C'est le mode **rootless** (celui de Podman) : aucun `sudo` nécessaire.

> **À retenir** : un namespace = une vue isolée d'une ressource.
> Le namespace user rend tout le reste possible sans être root.

---

## 2. Étape 2 : PID et mount

### Essai 1 : le namespace PID seul ne suffit pas

```
mon PID : 1                        ← le shell se croit PID 1…
processus visibles par ps : 476    ← …mais ps voit toute la machine ?!
```

Parce que **`ps` ne demande rien au noyau : il lit le dossier `/proc`**. Et ce
`/proc` était encore celui de l'hôte.

### `/proc` n'est pas un vrai dossier

```
0 octets  /proc/meminfo     ← "vide" sur le disque…
MemTotal: 15693872 kB       ← …mais affiche ta RAM
```

`/proc` est un **système de fichiers virtuel** : rien n'est stocké nulle part.
Chaque fois qu'un programme lit un fichier de `/proc`, **le noyau fabrique le
contenu à la volée**.

```
   ps ──── lit /proc/123/status ────►  NOYAU
                                          │  "je génère la réponse
   ps ◄─── "Name: sleep, State: S…" ──────┘   maintenant, à partir de
                                              mes structures internes"
```

### La solution : un `/proc` neuf, dans un namespace mount

On demande au noyau une **nouvelle instance** de procfs, liée à notre
namespace PID. Ce n'est **pas une copie** : le noyau génère son contenu en
direct, mais uniquement pour les processus de notre namespace.

Pour ne pas écraser le `/proc` de ton PC, il faut d'abord un **namespace
mount** : le conteneur a sa propre table de montages.

```
          table de l'HÔTE                  table du CONTENEUR
   ┌──────────────────────────┐    ┌──────────────────────────────┐
   │ /      → disque          │    │ /      → disque              │
   │ /proc  → procfs (hôte)   │    │ /proc  → procfs (hôte)       │ caché
   │ ...                      │    │ /proc  → procfs NEUF         │ ◄ visible
   └──────────────────────────┘    └──────────────────────────────┘
        inchangée                     le dernier montage recouvre
                                      le précédent
```

Résultat :

```
mon PID : 1
    PID USER     CMD
      1 root     ps -o pid,user,cmd      ← il ne voit plus que lui-même
--- /proc de l'hôte intact : 476 processus
```

### Un processus, deux numéros

Il n'y a **qu'un seul processus**, mais il a un numéro dans chaque namespace
PID où il est visible :

```
                 namespace PID de l'hôte
   ┌───────────────────────────────────────────────────┐
   │  1 systemd   …   150027 minibox   155657 sleep ◄──┼── même
   │                                     │             │   processus
   │       namespace PID du conteneur    │             │
   │      ┌──────────────────────────────┼─────┐       │
   │      │  1 sh          7 sleep ◄─────┘     │       │
   │      └────────────────────────────────────┘       │
   └───────────────────────────────────────────────────┘

$ grep NSpid /proc/155657/status
NSpid:  155657  7
```

L'hôte voit tout. Le conteneur ne voit que sa propre numérotation.

### Le mystère des 10 secondes du TD2

```
sleep normal :              arrêté par SIGTERM
sleep en PID 1 (minibox) :  TOUJOURS VIVANT après SIGTERM → seul SIGKILL l'arrête
```

Le noyau **protège le PID 1** (normalement `systemd`) : un signal ne lui est
transmis que s'il a prévu un gestionnaire pour ce signal. Un programme qui ne
gère pas `SIGTERM` l'ignore donc s'il est PID 1.

```
docker stop  ──SIGTERM──►  node (PID 1, pas de gestionnaire)  →  ignoré
     … 10 secondes …
docker stop  ──SIGKILL──►  node                                →  tué
```

Corrections possibles : gérer `SIGTERM` dans le code, ou `docker run --init`,
qui met un mini-init (`tini`) en PID 1 pour relayer les signaux.

### Le lien avec la question du TD2 Q4

`/proc/meminfo` est généré par le même noyau, et **il n'existe pas de
namespace mémoire**. Un conteneur voit donc toute la RAM de la machine, même
limité à 256 Mo. La limite (cgroups, étape 5) est réelle, mais invisible
dans `/proc`.

> **À retenir** : `/proc` est une fenêtre sur le noyau, pas un dossier.
> Isoler les processus = nouvelle numérotation (PID) + nouveau `/proc`
> (mount). Le PID 1 a un statut spécial pour les signaux.

---

## 3. Étape 3 : les fichiers de l'image

### Le problème

Après l'étape 2, le conteneur voyait encore **tous les fichiers de ton PC** :
ton `/home`, ton Node, ta configuration. Il dépendait donc de ce qui est
installé sur ta machine.

### Une image, c'est un dossier

```
$ ./export-rootfs.sh crazzybuilds/sysinfo-api:0.0.2 rootfs/sysinfo
crazzybuilds/sysinfo-api:0.0.2 extrait dans rootfs/sysinfo (65M)

rootfs/sysinfo/
├── app/            ← ton API (dist/, node_modules/, package.json)
├── bin/ lib/ usr/  ← Alpine et Node
├── etc/            ← la configuration d'Alpine
└── home/node/      ← l'utilisateur de l'image
```

Plus une configuration à part (`Cmd=[node dist/index.js]`, `WorkingDir=/app`,
`Env=PATH=…`).

### L'objectif : que ce dossier devienne `/`

```
  Avant : le conteneur voit            Après : le conteneur voit
  /         ← ton PC (Arch)            /          ← rootfs/sysinfo (Alpine)
  ├── home/nathan/…                    ├── app/
  ├── usr/bin/node (le tien)           ├── usr/bin/node (celui de l'image)
  └── etc/ (ta config)                 ├── etc/ (config d'Alpine)
                                       └── home/node/
```

Le programme ne sait pas qu'il est dans un sous-dossier : pour lui, `/` **est**
l'image.

### Comment : on modifie la table de montages du conteneur

**On ne touche jamais à ton ordinateur.** Tout se passe dans la table de
montages **du conteneur**, celle qu'il a reçue à l'étape 2 (namespace mount).

**① Bind mount** : `pivot_root` exige que la nouvelle racine soit un *point
de montage*, pas un simple dossier. On monte donc le dossier **sur lui-même**.
Rien n'est copié : c'est une « fenêtre » sur le même dossier, mais le noyau la
traite comme un montage à part entière, comme un disque.

```
  table du conteneur
  /                → disque (Arch)
  /…/rootfs/sysinfo → /…/rootfs/sysinfo   (bind : même dossier, vu comme un montage)
```

**② pivot_root** : on échange les deux lignes. L'image devient `/`, et
l'ancienne racine est rangée dans `/.oldroot`.

```
  table du conteneur
  /          → rootfs/sysinfo (Alpine)
  /.oldroot  → disque (Arch)               ← ton PC, encore accessible
```

**③ On emprunte ce qui manque** : un `/proc` neuf, et quelques périphériques
pris dans `/.oldroot/dev` (`null`, `zero`, `random`, `urandom`, `tty`). Sans
être vraiment root, on ne peut pas en créer, mais on peut monter ceux de
l'hôte.

**④ On coupe** : on démonte `/.oldroot`. Plus aucune ligne de la table ne mène
à ton PC.

```
  table du conteneur (finale)            table de l'hôte (jamais modifiée)
  /             → rootfs/sysinfo          /  → /dev/mapper/root
  /proc         → procfs neuf             …
  /dev/null     → /dev/null de l'hôte
  /dev/urandom  → /dev/urandom de l'hôte
  …
```

Vérifié sur la machine :

```
--- table des montages DANS le conteneur :
  /             (source: /@home/nathan/Projects/minibox/rootfs/sysinfo)
  /proc
  /dev/null, /dev/zero, /dev/random, /dev/urandom, /dev/tty
--- la racine de l'hôte :
  /             /dev/mapper/root           ← inchangée
```

### Pourquoi `pivot_root` et pas `chroot` ?

`chroot` change seulement **le point de départ des chemins** d'un processus :
l'ancienne racine existe toujours dans la table, et un processus root peut en
ressortir. `pivot_root` **déplace réellement la racine** du namespace mount,
et on supprime ensuite l'ancienne : il n'y a plus rien vers quoi s'échapper.
C'est ce qu'utilise `runc`.

### Le résultat : l'API tourne sans Docker

```
$ ./minibox run -rootfs rootfs/sysinfo -workdir /app node dist/index.js
Server listening on http://localhost:8000

$ curl localhost:8000/api/v1/sysinfo        (depuis l'hôte)
HTTP 200
os     : Alpine Linux v3.20 | hostname: minibox | noyau: 7.2.15-4-omarchy
procs  : 2 processus
mem    : 15.0 Go
cpu    : Ryzen 5 5500H with Radeon Graphics 8 threads
```

| Ce que voit l'API | Pourquoi |
|---|---|
| Alpine 3.20 (le PC est sous Arch) | les fichiers sont ceux de l'image (étape 3) |
| hostname `minibox` | namespace UTS (étape 1) |
| 2 processus | namespace PID + `/proc` neuf (étape 2) |
| noyau `omarchy` | **le noyau est partagé** : un conteneur n'a pas de noyau à lui |
| 15 Go, 8 threads | pas encore de limite, pas de namespace mémoire (étape 5) |

`curl localhost:8000` fonctionne depuis l'hôte parce que le **namespace réseau
est encore partagé** (étape 6).

### La limite qui mène à l'étape 4

Le conteneur écrit **directement dans le dossier de l'image** :

```
dans le conteneur :  echo coucou > /tmp/fichier
sur l'hôte :         rootfs/sysinfo/tmp/fichier   ← il est là, propriétaire nathan
```

Deux conteneurs lancés depuis ce dossier se marcheraient dessus, et l'image se
salirait. Docker ne recopie jamais l'image : il **empile ses couches en lecture
seule** et ajoute **une couche inscriptible par conteneur** (overlayfs).

> **À retenir** : une image = un dossier + une config. `pivot_root` en fait la
> racine du conteneur, en ne modifiant **que la table de montages du
> conteneur**. Seul le noyau vient de l'hôte, c'est ce qui rend les
> conteneurs portables.

---

## 4. Étape 4 : overlayfs, les couches de l'image

### Une image, ce sont des couches

`docker save` montre ce qu'est vraiment une image : **une pile d'archives**
(une par instruction du Dockerfile qui modifie des fichiers) et **une
configuration**.

```
$ ./pull-image.sh crazzybuilds/sysinfo-api:0.0.2 images/sysinfo
couche 00 : 9.1M   517 fichiers   ← FROM alpine:3.20
couche 01 : 0        1 fichiers   ← WORKDIR /app
couche 02 : 55M    413 fichiers   ← RUN apk add nodejs
couche 03 : 24K     10 fichiers   ← RUN addgroup / adduser
couche 04 : 4.0K     2 fichiers   ← COPY package.json
couche 05 : 908K    54 fichiers   ← COPY node_modules
couche 06 : 80K     15 fichiers   ← COPY dist

config.json : {"Cmd":["node","dist/index.js"], "WorkingDir":"/app", "Env":[…], "User":"node"}
```

Ce sont les couches que montrait `dive` au TD2. Chacune ne contient **que ce
que son instruction a ajouté ou modifié**.

### Ce que fait overlayfs : empiler des dossiers

overlayfs est un système de fichiers du noyau qui **superpose plusieurs
dossiers** et présente le résultat comme un seul.

```mermaid
flowchart BT
    L0["layers/00 : Alpine"] --> L1["layers/01 : WORKDIR"]
    L1 --> L2["layers/02 : Node.js"]
    L2 --> L3["layers/03 : utilisateur node"]
    L3 --> L4["layers/04..06 : ton API"]
    L4 --> U["upper : écritures de CE conteneur ✏️"]
    U --> M["merged : ce que voit le conteneur 👀"]

    style L0 fill:#ddd,color:#000
    style L1 fill:#ddd,color:#000
    style L2 fill:#ddd,color:#000
    style L3 fill:#ddd,color:#000
    style L4 fill:#ddd,color:#000
    style U fill:#ffe08a,color:#000
    style M fill:#9fd8a8,color:#000
```

| Dossier | Rôle | Accès |
|---|---|---|
| `lowerdir` (les 7 couches) | l'image | **lecture seule**, partagées par tous les conteneurs |
| `upperdir` | les modifications de **ce** conteneur | lecture-écriture |
| `workdir` | brouillon interne d'overlayfs | — |
| `merged` | la vue fusionnée, qui devient `/` du conteneur | ce que voit le programme |

Pour lire un fichier, overlayfs le cherche **de haut en bas** et prend le
premier trouvé : si `upper` en a une version, c'est elle, sinon on descend
dans les couches.

### Les trois cas d'écriture

```mermaid
flowchart LR
    subgraph Créer
      A1["echo salut > /app/note.txt"] --> A2["note.txt apparaît<br/>dans upper uniquement"]
    end
    subgraph Modifier
      B1["echo x >> /etc/motd"] --> B2["copy-up : motd est copié<br/>ENTIER dans upper, puis modifié"]
    end
    subgraph Supprimer
      C1["rm /bin/ls"] --> C2["whiteout : un marqueur<br/>« supprimé » dans upper"]
    end
```

Vérifié sur la machine, dans le `upper` du conteneur A :

```
./app/note.txt     regular file              ← créé
./etc/motd         regular file (11 lignes)  ← copie de l'original (10 lignes) + modif
./bin/ls           character special 0,0     ← whiteout : "ce fichier n'existe plus"
```

Le fichier original n'est **jamais** touché. Supprimer un fichier ne libère
donc aucune place : il reste dans sa couche, simplement masqué. C'est pour ça
que supprimer des fichiers dans un `RUN` à part ne réduit pas une image
Docker.

### Deux conteneurs, une seule image

```mermaid
flowchart BT
    IMG["couches de l'image (65 Mo)<br/>lecture seule, partagées"]
    IMG --> UA["upper de A (8 Ko)<br/>note.txt, motd, whiteout ls"]
    IMG --> UB["upper de B<br/>vide"]
    UA --> MA["/ vu par A"]
    UB --> MB["/ vu par B"]
```

```
=== conteneur B, même image :
cat: can't open '/app/note.txt'   ← il ne voit pas le fichier de A
/bin/ls                           ← ls existe toujours pour lui
```

C'est ce qui rend Docker rapide et économe :
- **démarrer un conteneur ne copie rien** : on crée trois dossiers vides et on
  monte un overlay ;
- **10 conteneurs de la même image** = 65 Mo d'image + 10 petits `upper` ;
- **des images différentes partagent leurs couches communes** : c'était le
  `Already exists` du `docker pull` de la Q8 (la couche Alpine était déjà là).

### Le montage, en une ligne

Le montage se fait **dans `child`**, donc dans le namespace mount du conteneur
(l'hôte ne le voit pas) :

```
overlay on / type overlay (lowerdir=…/layers/06:…/layers/05:…:…/layers/00,
                           upperdir=…/containers/ecc20b/upper,
                           workdir=…/containers/ecc20b/work,
                           userxattr)
```

- `lowerdir` : la couche **du dessus en premier** (d'où l'inversion dans le
  code) ;
- `userxattr` : l'option qui permet d'utiliser overlayfs **sans être root**
  (dans un user namespace).

### Comme `docker run`, sans commande

minibox lit maintenant `config.json` : sans commande, il lance le `Cmd` de
l'image, dans son `WorkingDir`, avec son `Env`.

```
$ ./minibox run -image images/sysinfo
minibox : conteneur f9a779 (containers/f9a779)
Server listening on http://localhost:8000
```

> **À retenir** : une image = des couches en lecture seule + une config. Un
> conteneur = ces couches + **une couche inscriptible à lui**, fusionnées par
> overlayfs. Écrire ne modifie jamais l'image : créer → dans `upper`,
> modifier → copie dans `upper`, supprimer → marqueur dans `upper`.

---

## 5. Étape 5 : cgroups, limiter ce que le conteneur consomme

### Namespaces et cgroups : deux mécanismes différents

```mermaid
flowchart LR
    P["processus du conteneur"]
    P --> NS["namespaces<br/>👓 ce qu'il VOIT"]
    P --> CG["cgroups<br/>⚖️ ce qu'il peut CONSOMMER"]
    NS --> N1["hostname, processus,<br/>fichiers, réseau…"]
    CG --> C1["mémoire, CPU,<br/>nombre de processus…"]
```

Jusqu'ici, le conteneur était **isolé** (il ne voit pas le reste) mais **pas
limité** : il pouvait prendre toute la RAM et tout le CPU de la machine. Les
**cgroups** (*control groups*) servent à ça : le noyau compte ce que consomme
un groupe de processus, et l'empêche de dépasser des limites.

### Un cgroup, c'est un dossier

Comme `/proc`, les cgroups s'utilisent **avec des fichiers** : chaque cgroup
est un dossier de `/sys/fs/cgroup`, chaque limite un fichier qu'on écrit.

```
/sys/fs/cgroup/                                    ← racine (root uniquement)
└── user.slice/user-1000.slice/
    └── user@1000.service/                         ← délégué par systemd : appartient à nathan
        ├── app.slice/…                            ← tes applis (terminal, navigateur…)
        └── minibox/                               ← créé par minibox
            ├── cgroup.subtree_control  "+cpu +memory +pids"
            └── 2e0a5f/                            ← un conteneur
                ├── cgroup.procs        les PID qui en font partie
                ├── memory.max          268435456       (256 Mo)
                ├── memory.swap.max     0               (pas de swap)
                ├── cpu.max             50000 100000    (0.5 CPU)
                ├── pids.max            64
                ├── memory.current      ← lu : consommation actuelle (12 Mo)
                └── memory.events       ← lu : "oom_kill 1" si le noyau a tué
```

Tout le travail de `createCgroup` consiste à **créer un dossier et écrire
dans des fichiers**. Pas besoin d'être root : systemd te délègue ta branche
(`user@1000.service`), qui t'appartient.

Le processus doit ensuite **entrer dans le cgroup**. minibox utilise
`CLONE_INTO_CGROUP` : l'enfant **naît directement dedans**, il est donc limité
dès sa première instruction.

### Mémoire : le noyau tue au-delà de la limite

```
$ ./minibox run -image images/sysinfo -memory 256M /bin/sh -c '…node qui alloue 10 Mo en boucle…'
MemTotal vu par le conteneur : MemTotal: 15693872 kB     ← il croit avoir 15 Go…
alloué : 50 Mo
alloué : 100 Mo
alloué : 150 Mo
alloué : 200 Mo
minibox : le noyau a tué 1 processus du conteneur : limite mémoire dépassée (OOM)
code de sortie : 137                                     ← 128 + 9 (SIGKILL)
```

```mermaid
sequenceDiagram
    participant N as node (conteneur)
    participant K as noyau
    N->>K: alloue 10 Mo
    K->>K: memory.current + 10 Mo ≤ memory.max ?
    K-->>N: oui, voilà
    Note over N,K: … 25 fois …
    N->>K: alloue 10 Mo
    K->>K: 256 Mo dépassés, swap interdit
    K-->>N: SIGKILL (OOM killer)
    Note over K: memory.events : oom_kill 1
```

Le code **137** est celui que Docker affiche pour un conteneur `OOMKilled`.

Sans `memory.swap.max = 0`, le conteneur n'aurait pas été tué : le noyau
aurait envoyé l'excédent dans le swap (30 Go sur cette machine), et le
programme serait devenu très lent au lieu de s'arrêter.

### CPU : le noyau rationne le temps de calcul

`cpu.max = "50000 100000"` : **50 ms de calcul autorisées toutes les 100 ms**.
Au-delà, le processus est mis en pause jusqu'à la période suivante
(*throttling*).

```
un calcul qui tourne pendant 3 s :
sans limite      → CPU réellement obtenu : 2976 ms   (100 %)
-cpus 0.5        → CPU réellement obtenu : 1498 ms   ( 50 %)
-cpus 0.25       → CPU réellement obtenu :  750 ms   ( 25 %)
```

```
-cpus 0.5, sur une période de 100 ms :
|██████████░░░░░░░░░░|██████████░░░░░░░░░░|██████████░░░░░░░░░░|
 50 ms     50 ms      calcul    en pause
 calcul    en pause
```

Le programme ne sait pas qu'il est limité : il est juste plus lent.

### Nombre de processus : la protection contre les fork bombs

```
$ ./minibox run -image images/sysinfo -pids 10 /bin/sh -c '…lance 20 sleep…'
/bin/sh: can't fork: Resource temporarily unavailable
processus vivants dans le conteneur : 8 (limite : 10)
```

Sans cette limite, un programme qui se duplique à l'infini (*fork bomb*)
pourrait saturer la table des processus de **toute la machine**.

### La réponse à la question du TD2 Q4

Ton API, limitée à 256 Mo et 0,5 CPU :

| | Ce que dit `sysinfo` | La réalité (fichiers du cgroup, vus de l'hôte) |
|---|---|---|
| mémoire | `mem.total` : **15 Go** | `memory.max` : **256 Mo** |
| CPU | `cpu.cores` : **8** | `cpu.max` : **0,5 CPU** |

```mermaid
flowchart TB
    API["sysinfo-api"] -->|lit| PROC["/proc/meminfo, /proc/cpuinfo"]
    PROC -->|généré pour| HOST["la machine entière<br/>15 Go, 8 threads"]
    K["noyau"] -->|applique| CG["cgroup<br/>256 Mo, 0.5 CPU"]
    CG -.->|limite réellement| API
    PROC ~~~ CG
```

Les deux informations viennent du même noyau, mais **par deux chemins
différents** :
- `/proc/meminfo` décrit **la machine** (il n'existe pas de namespace
  mémoire) ;
- la limite vit dans **le cgroup**, que `sysinfo` ne lit pas.

La limite est donc bien **réelle** (le noyau a tué le programme à 256 Mo),
mais **invisible** pour un programme qui ne regarde que `/proc`. C'est un vrai
problème en production : un programme qui dimensionne ses caches ou son nombre
de threads d'après `/proc` croit avoir 15 Go et 8 cœurs, et se fait tuer.
C'est pour ça que Node.js, Java ou Go lisent désormais **aussi** les fichiers
de leur cgroup pour connaître leurs vraies limites.

Sur Azure (TD4), le problème n'existait pas : la micro-VM avait **vraiment**
1 vCPU et 1,5 Go, donc `/proc` disait vrai.

> **À retenir** : namespaces = ce que le processus **voit**, cgroups = ce
> qu'il peut **consommer**. Un cgroup est un dossier, une limite un fichier.
> Les limites sont réelles mais **invisibles dans `/proc`**.

---

## 6. Étape 6 : le réseau

### Un namespace réseau neuf : une pile réseau vide

Avec `CLONE_NEWNET`, le conteneur reçoit **sa propre pile réseau** : ses
interfaces, ses adresses, sa table de routage, et **ses propres ports**.
Au départ, elle est vide.

```
=== hôte :       lo, enp2s0, wlo1 (10.17.213.206), br-… (le réseau de Docker)
=== conteneur :  lo 127.0.0.1/8            ← et c'est tout
ping 1.1.1.1 :   Network unreachable
```

Même `lo` est **éteinte** dans un namespace neuf : minibox l'allume avec un
`ioctl` (l'équivalent de `ip link set lo up`).

### Deux `localhost` différents

```
API dans le conteneur :          Server listening on http://localhost:8000
curl localhost:8000 (hôte) :     connexion impossible
netns de l'API : net:[4026534247] | hôte : net:[4026531833]
```

```mermaid
flowchart LR
    subgraph H["hôte · netns 4026531833"]
      C["curl localhost:8000"] --> LH["lo de l'hôte<br/>port 8000 : personne"]
    end
    subgraph K["conteneur · netns 4026534247"]
      LC["lo du conteneur<br/>port 8000 : node ✅"]
    end
    LH -. aucun lien .- LC
```

`localhost` désigne **la pile réseau du namespace courant**. L'API écoute sur
celui du conteneur, `curl` frappe à celui de l'hôte. C'est pour ça qu'une API
dans Docker ne répond pas sans `-p 8000:8000`.

### Comment Docker relie le conteneur (en root)

```mermaid
flowchart LR
    subgraph conteneur
      E1["eth0<br/>172.18.0.2"]
    end
    E1 ===|"paire veth<br/>(câble virtuel)"| V["veth… "]
    V --- BR["pont br-…<br/>172.18.0.1"]
    BR --> NAT["règles de pare-feu<br/>NAT, -p 8000:8000"] --> NET["wlo1 → Internet"]
```

Une **paire veth** est un câble virtuel : un bout dans le conteneur, l'autre
branché sur un pont de l'hôte (le `br-…` de la liste plus haut). Des règles de
pare-feu traduisent les adresses et redirigent les ports publiés.
**Tout ça demande root** : créer des interfaces et des règles sur l'hôte.

### Comment minibox le relie (sans root) : pasta

**pasta** (projet **passt**, utilisé par Podman) est un **programme
ordinaire** qui :
1. rejoint le namespace réseau du conteneur et y crée une interface ;
2. copie la configuration de l'hôte (adresse, route par défaut) ;
3. **relaie** chaque connexion à travers des **sockets normales de l'hôte**,
   comme le ferait n'importe quel programme qui se connecte à Internet.

```mermaid
flowchart LR
    subgraph conteneur
      N["node :8000"] --- T["interface tap<br/>10.17.213.206"]
    end
    T <--> P["pasta<br/>(processus de nathan)"]
    P <-->|"sockets ordinaires"| HS["pile réseau de l'hôte"]
    CU["curl localhost:8123"] --> HS
    HS --> I["Internet"]
```

Pour Internet, c'est pasta qui se connecte. Pour un port publié, c'est pasta
qui **écoute sur le port de l'hôte** et retransmet à l'intérieur.

### Le tube de synchronisation

Pour brancher le réseau, il faut agir **de l'extérieur** sur un conteneur
**qui existe déjà** (pasta a besoin de son PID). Mais la commande ne doit pas
démarrer avant que le réseau soit prêt. D'où un **tube** entre parent et
enfant :

```mermaid
sequenceDiagram
    participant R as minibox run (hôte)
    participant C as minibox child (conteneur)
    participant P as pasta
    R->>C: clone() avec CLONE_NEWNET
    Note over C: bloqué : lit le tube (fd 3)
    R->>P: pasta --config-net -t 8123:8000 <PID>
    P->>C: rejoint son netns, crée l'interface
    P-->>R: prêt (passe en arrière-plan)
    R->>C: ferme le tube = feu vert
    Note over C: lo up, montages, pivot_root…
    C->>C: exec("node dist/index.js")
```

`runc` utilise exactement ce mécanisme entre ses différentes phases.

### Le DNS : le piège du 127.0.0.53

Premier essai : `ping 1.1.1.1` passe, mais `wget example.com` échoue.

- L'image **n'a pas** de `/etc/resolv.conf` : Docker l'**injecte** au
  lancement.
- Ton PC utilise `nameserver 127.0.0.53` (systemd-resolved) : une adresse de
  **son** `localhost`, donc injoignable depuis le conteneur.

Solution (celle de Podman) : `pasta --dns-forward 169.254.1.1` relaie vers le
DNS de l'hôte tout ce qui est envoyé à `169.254.1.1` dans le conteneur, et
minibox écrit `nameserver 169.254.1.1` dans le `resolv.conf` du conteneur.

```
resolv.conf : nameserver 169.254.1.1
wget example.com : <title>Example Domain</title>
```

### Le résultat : l'équivalent de `docker run -p 8123:8000`

```
$ ./minibox run -image images/sysinfo -memory 256M -fake-meminfo -p 8123:8000
Server listening on http://localhost:8000

depuis l'hôte :
  localhost:8000 → connexion impossible        ← le port du conteneur n'est pas celui de l'hôte
  localhost:8123 → hostname minibox | Alpine Linux | mem 256 Mo | 2 processus
après l'arrêt du conteneur : pasta est parti avec le namespace
```

| Option | Réseau du conteneur |
|---|---|
| `-net none` (défaut) | isolé : seulement `lo` |
| `-net pasta` | connecté à Internet via pasta |
| `-p 8123:8000` | pasta + port 8123 de l'hôte → port 8000 du conteneur |
| `-net host` | celui de l'hôte (comme aux étapes 1 à 5, et `docker run --network host`) |

> **À retenir** : un namespace réseau = une pile réseau complète, vide au
> départ, avec **son propre `localhost` et ses propres ports**. En root,
> Docker le relie avec un câble virtuel (veth) et un pont ; sans root, pasta
> relaie le trafic à travers des sockets ordinaires.

---

## 7. Étape 7 (bonus) : faire voir ses limites au conteneur

L'étape 5 a laissé un problème : l'API est limitée à 256 Mo, mais `sysinfo`
annonce 15 Go. Deux solutions existent dans le monde réel, et elles ne
répondent pas au même besoin.

```mermaid
flowchart TB
    Q["Le programme lit ses ressources"] --> A
    Q --> B
    subgraph A["A · méthode Docker"]
      A1["namespace cgroup<br/>+ /sys/fs/cgroup monté"] --> A2["le programme PEUT lire<br/>memory.max s'il le cherche"]
    end
    subgraph B["B · méthode lxcfs"]
      B1["/proc/meminfo recouvert<br/>par un faux fichier"] --> B2["le programme voit 256 Mo<br/>sans rien changer"]
    end
```

### A. Le namespace cgroup : le conteneur peut trouver ses limites

Deux ajouts :
1. `CLONE_NEWCGROUP` : le cgroup du conteneur lui apparaît comme **la racine**
   de l'arbre des cgroups ;
2. le système de fichiers des cgroups est monté dans `/sys/fs/cgroup` : le
   conteneur y lit **ses propres** fichiers.

```
                 vu de l'hôte                              vu du conteneur
/sys/fs/cgroup/                                      /sys/fs/cgroup/   ← sa racine
└── user.slice/…/user@1000.service/                      ├── memory.max   268435456
    └── minibox/                                         ├── cpu.max      50000 100000
        └── 2e0a5f/   ◄────────── même dossier ────────► └── …
            ├── memory.max
            └── cpu.max
```

Résultat sur la machine :

```
/proc/self/cgroup : 0::/                       ← avant : 0::/user.slice/user-1000.slice/…/minibox/c524ac
memory.max        : 268435456
cpu.max           : 50000 100000
node os.totalmem()             : 15326 Mo      ← lit /proc/meminfo
node process.constrainedMemory(): 256 Mo       ← lit le cgroup
node os.availableParallelism()  : 8
MemTotal (/proc/meminfo)        : 15693872 kB
```

Deux gains :
- **sécurité** : le conteneur ne découvre plus le chemin de son cgroup sur
  l'hôte (avant, `/proc/self/cgroup` révélait `user-1000`, le nom du service…) ;
- **les limites deviennent lisibles**, mais seulement pour un programme qui
  va les chercher : c'est ce que fait `process.constrainedMemory()` de Node,
  ou la JVM depuis Java 10. `sysinfo`, lui, ne lit que `/proc` et voit
  toujours 15 Go.

C'est ce que fait Docker par défaut.

### B. Le faux `/proc/meminfo` : le conteneur voit ses limites sans le savoir

Avec `-fake-meminfo`, minibox :
1. lit la limite dans `/sys/fs/cgroup/memory.max` (grâce à A) ;
2. écrit un fichier `meminfo` qui annonce cette limite ;
3. le monte **par-dessus** `/proc/meminfo` (bind mount sur un seul fichier).

```
/proc/
├── cpuinfo     ← vrai (généré par le noyau)
├── meminfo     ← RECOUVERT par /run/minibox-meminfo :
│                    MemTotal:     262144 kB
│                    MemFree:      258xxx kB
│                    SwapTotal:    0 kB
├── 1/ …        ← vrais
```

Résultat, avec **la même API, sans modifier son code** :

```
-memory 256M :                 mem.total : 15326 Mo | swap : 30656 Mo | cpu.cores : 8
-memory 256M -fake-meminfo :   mem.total :   256 Mo | swap :     0 Mo | cpu.cores : 8
```

C'est le principe de **lxcfs**, utilisé par LXC et certains clusters
Kubernetes, pour que les vieux programmes (qui ne connaissent pas les
cgroups) se dimensionnent correctement.

### La limite de notre version : une photo, pas une fenêtre

```
au démarrage :              MemFree : 251 Mo  |  memory.current : 8 Mo utilisés
après avoir alloué 150 Mo : MemFree : 251 Mo  |  memory.current : 159 Mo utilisés
                                      ▲
                                      ne bouge pas : c'est un fichier écrit une fois
```

Le vrai `/proc/meminfo` est **généré à chaque lecture** (étape 2). Notre faux
fichier, lui, est écrit **une seule fois**. lxcfs résout ça avec **FUSE** : un
système de fichiers en espace utilisateur, où **un programme** (au lieu du
noyau) fabrique le contenu à chaque lecture, en consultant le cgroup.

```mermaid
flowchart LR
    subgraph notre version
      P1["lecture de /proc/meminfo"] --> F1["fichier écrit au démarrage"]
    end
    subgraph lxcfs
      P2["lecture de /proc/meminfo"] --> K["noyau"] --> D["démon lxcfs (FUSE)"] --> C["lit le cgroup<br/>maintenant"]
    end
```

### Et le CPU ?

`cpu.cores` affiche toujours 8 : on n'a pas touché à `/proc/cpuinfo` ni
`/proc/stat`. C'est plus compliqué, car « 0,5 CPU » n'est pas un nombre de
cœurs : c'est un quota de temps (50 ms toutes les 100 ms), sur **tous** les
cœurs. lxcfs arrondit (0,5 → 1 cœur) et réécrit aussi ces fichiers.

À comparer avec Azure (TD4) : la micro-VM avait **vraiment** 1 vCPU, donc
rien à truquer.

| Méthode | Ce que voit `sysinfo` | Coût |
|---|---|---|
| rien (étape 5) | 15 Go, 8 cœurs | — |
| A : namespace cgroup (Docker) | 15 Go, 8 cœurs (mais limites lisibles dans `/sys/fs/cgroup`) | aucun |
| B : faux `/proc` (lxcfs) | 256 Mo (8 cœurs ici, 1 avec lxcfs) | un démon FUSE sur l'hôte |
| micro-VM (Azure) | 1,5 Go, 1 cœur : **la vérité** | un noyau et une VM par conteneur |

> **À retenir** : un conteneur ne peut pas « voir » ses limites dans `/proc`,
> parce que `/proc` décrit la machine. Soit le programme lit son cgroup
> (méthode Docker), soit on lui ment dans `/proc` (méthode lxcfs), soit on
> lui donne une vraie machine plus petite (micro-VM).

---

## Récapitulatif

```
  ./minibox run -image images/sysinfo -memory 256M -cpus 0.5 -fake-meminfo -p 8123:8000
     │
     ├─ lit config.json (Cmd, WorkingDir, Env)                 (run)
     ├─ crée containers/<id>/{upper,work,merged}
     ├─ crée le cgroup minibox/<id> et écrit ses limites
     ├─ clone() avec CLONE_NEWUSER, NEWUTS, NEWPID, NEWNS, NEWCGROUP, NEWNET
     │          et CLONE_INTO_CGROUP (né dans son cgroup)
     ├─ pasta --config-net -t 8123:8000 <PID>        réseau branché de l'extérieur
     ├─ ferme le tube de synchronisation             feu vert pour l'enfant
     │
     └─ dans l'enfant :                                         (child)
          ├─ attend le feu vert (tube, fd 3)
          ├─ lo up                          namespace réseau
          ├─ uid 1000 → uid 0               namespace user
          ├─ sethostname("minibox")         namespace UTS
          ├─ montages privés                namespace mount
          ├─ mount overlay → merged         couches + upper
          ├─ bind + pivot_root sur merged   fichiers de l'image
          ├─ /proc neuf, /sys/fs/cgroup, /dev empruntés
          ├─ démontage de l'ancienne racine
          ├─ (option) faux /proc/meminfo    façon lxcfs
          ├─ /etc/resolv.conf → 169.254.1.1 DNS relayé par pasta
          ├─ chdir /app + environnement de l'image
          └─ exec("node dist/index.js")     → PID 1 du conteneur
```

## Petit glossaire

| Terme | Définition courte |
|---|---|
| **noyau** | le cœur de Linux, qui gère processus, mémoire, fichiers, réseau. Un seul pour toute la machine |
| **processus** | un programme en cours d'exécution, avec un numéro (PID) |
| **namespace** | une vue isolée d'une ressource du noyau, donnée à un groupe de processus |
| **clone()** | l'appel système qui crée un processus, éventuellement dans de nouveaux namespaces |
| **exec()** | remplace le programme d'un processus par un autre, sans changer de PID |
| **montage** | une ligne de la table « tel dossier affiche tel contenu » |
| **bind mount** | monter un dossier à un autre endroit (ou sur lui-même), sans copie |
| **procfs** | le système de fichiers virtuel de `/proc`, généré par le noyau à chaque lecture |
| **pivot_root** | échange la racine du namespace mount avec un autre point de montage |
| **rootless** | conteneurs lancés sans être root, grâce au namespace user |
| **overlayfs** | système de fichiers qui superpose des dossiers (couches) en une seule vue |
| **copy-up** | copie d'un fichier dans `upper` au moment où le conteneur le modifie |
| **whiteout** | marqueur qui signifie « ce fichier est supprimé » dans une couche |
| **cgroup** | groupe de processus dont le noyau compte et limite la consommation |
| **OOM killer** | le mécanisme du noyau qui tue un processus quand la mémoire manque |
| **throttling** | mise en pause d'un processus qui a épuisé son quota de CPU |
| **lxcfs** | outil qui remplace des fichiers de `/proc` par des versions qui reflètent les limites du cgroup |
| **veth** | paire d'interfaces virtuelles reliées comme un câble : un bout dans le conteneur, l'autre sur l'hôte |
| **pasta** | programme qui relie un namespace réseau à l'hôte sans root, en relayant le trafic par des sockets ordinaires |
| **FUSE** | système de fichiers dont le contenu est fabriqué par un programme, et non par le noyau |

---

## Pour vérifier que c'est compris

<details>
<summary>1. Un conteneur Docker tourne-t-il dans une machine virtuelle ?</summary>

Non (sur Linux). C'est un processus de l'hôte, visible dans `ps`, qui partage
le noyau. Seules des solutions comme Azure (micro-VM) ou Docker Desktop sur
Mac/Windows ajoutent une VM.
</details>

<details>
<summary>2. Pourquoi minibox se relance-t-il lui-même au lieu de lancer directement la commande ?</summary>

Les namespaces se créent à la naissance d'un processus (`clone()`). L'enfant,
déjà dans les nouveaux namespaces, doit encore préparer l'intérieur (hostname,
montages, racine) **avant** de devenir la commande avec `exec()`.
</details>

<details>
<summary>3. Le conteneur est root : peut-il supprimer des fichiers système de mon PC ?</summary>

Non. Il est root seulement dans son namespace user. Pour le noyau, c'est
l'utilisateur 1000 : il n'a pas plus de droits que toi.
</details>

<details>
<summary>4. Pourquoi le namespace PID seul ne suffit-il pas à cacher les processus de l'hôte ?</summary>

Parce que `ps` lit `/proc`, et ce `/proc` était celui de l'hôte. Il faut monter
un procfs neuf, ce qui demande un namespace mount pour ne pas toucher celui de
l'hôte.
</details>

<details>
<summary>5. Quand on monte un nouveau /proc, est-ce qu'on copie les informations des processus ?</summary>

Non. `/proc` est virtuel : le noyau génère son contenu à chaque lecture. Le
nouveau procfs génère simplement la vue de notre namespace PID.
</details>

<details>
<summary>6. Pourquoi l'API ne s'arrête-t-elle pas avec un SIGTERM ?</summary>

Elle est PID 1 dans son namespace, et le noyau ne transmet un signal au PID 1
que s'il a un gestionnaire pour ce signal. Node n'en a pas pour `SIGTERM`.
</details>

<details>
<summary>7. Pendant pivot_root, la racine de mon PC est-elle déplacée ?</summary>

Non. Seule la table de montages **du conteneur** change. Pendant un instant,
ton PC y est visible dans `/.oldroot`, puis on supprime cette ligne. La table
de l'hôte n'est jamais modifiée.
</details>

<details>
<summary>8. Pourquoi faut-il monter le rootfs sur lui-même avant pivot_root ?</summary>

`pivot_root` exige que la nouvelle racine soit un point de montage. Le bind
mount transforme le dossier en point de montage, sans rien copier.
</details>

<details>
<summary>9. L'API dans minibox affiche le noyau "omarchy" et non un noyau Alpine. Pourquoi ?</summary>

Un conteneur n'a pas de noyau : seuls les fichiers viennent de l'image. Le
noyau est toujours celui de l'hôte.
</details>

<details>
<summary>10. Si deux conteneurs utilisent le même dossier rootfs/sysinfo, que se passe-t-il ?</summary>

Ils écrivent dans les mêmes fichiers et se marchent dessus, et l'image est
modifiée. C'est ce que règle overlayfs à l'étape 4.
</details>

<details>
<summary>11. Que contient une image Docker ?</summary>

Des couches (une archive par instruction du Dockerfile qui modifie des
fichiers) et une configuration (`Cmd`, `WorkingDir`, `Env`, `User`…). Pas de
noyau, pas de processus.
</details>

<details>
<summary>12. Le conteneur modifie /etc/motd. Qu'arrive-t-il au fichier de l'image ?</summary>

Rien. overlayfs copie d'abord le fichier entier dans `upper` (copy-up), et la
modification porte sur cette copie. Les couches de l'image restent en lecture
seule.
</details>

<details>
<summary>13. Pourquoi un `RUN rm gros-fichier` à part ne réduit-il pas la taille d'une image ?</summary>

Le fichier reste dans la couche qui l'a ajouté. La nouvelle couche ne contient
qu'un whiteout qui le masque. Il faut le supprimer dans le même `RUN` qui l'a
créé, ou utiliser un build multi-stage.
</details>

<details>
<summary>14. Pourquoi lancer un conteneur est-il presque instantané, même pour une grosse image ?</summary>

Rien n'est copié : on crée trois dossiers vides (`upper`, `work`, `merged`) et
on monte un overlay par-dessus les couches existantes.
</details>

<details>
<summary>15. Quelle est la différence entre un namespace et un cgroup ?</summary>

Un namespace change ce que le processus **voit** (hostname, processus,
fichiers…). Un cgroup limite ce qu'il peut **consommer** (mémoire, CPU,
nombre de processus).
</details>

<details>
<summary>16. Comment configure-t-on la limite mémoire d'un cgroup ?</summary>

En écrivant dans un fichier : `memory.max` du dossier du cgroup, dans
`/sys/fs/cgroup`. Un cgroup est un dossier, chaque limite un fichier.
</details>

<details>
<summary>17. Un conteneur limité à 256 Mo affiche 15 Go dans /proc/meminfo. La limite est-elle fausse ?</summary>

Non, elle est bien appliquée (le noyau tue le processus à 256 Mo). Mais
`/proc/meminfo` décrit la machine entière : la limite est dans le cgroup, que
le programme doit lire lui-même pour la connaître.
</details>

<details>
<summary>18. Que signifie le code de sortie 137 ?</summary>

128 + 9 : le processus a été tué par le signal 9 (`SIGKILL`). C'est ce
qu'envoie l'OOM killer quand la limite mémoire est dépassée.
</details>

<details>
<summary>19. Avec -cpus 0.5, le programme utilise-t-il un seul cœur à moitié ?</summary>

Pas forcément. Le noyau lui accorde 50 ms de calcul toutes les 100 ms, sur
n'importe quels cœurs. Au-delà, il est mis en pause jusqu'à la période
suivante (throttling).
</details>

<details>
<summary>20. Avec le namespace cgroup, que voit le conteneur dans /proc/self/cgroup, et pourquoi c'est mieux ?</summary>

`0::/` : son cgroup lui apparaît comme la racine. Il ne découvre plus le
chemin complet sur l'hôte (utilisateur, services…), et il peut lire ses
limites dans `/sys/fs/cgroup`.
</details>

<details>
<summary>21. Pourquoi os.totalmem() et process.constrainedMemory() de Node ne donnent-ils pas la même valeur ?</summary>

`os.totalmem()` lit `/proc/meminfo` (la machine : 15 Go).
`process.constrainedMemory()` lit le cgroup (la limite : 256 Mo).
</details>

<details>
<summary>22. Comment sysinfo peut-il afficher 256 Mo sans modifier son code ?</summary>

On monte un faux fichier par-dessus `/proc/meminfo` dans le namespace mount du
conteneur. `sysinfo` lit ce fichier en croyant lire celui du noyau.
</details>

<details>
<summary>23. Pourquoi notre faux meminfo devient-il faux au fil du temps, et comment lxcfs fait-il mieux ?</summary>

C'est un fichier écrit une fois au démarrage, alors que le vrai est généré à
chaque lecture. lxcfs utilise FUSE : un démon regénère le contenu à chaque
lecture, à partir des valeurs actuelles du cgroup.
</details>

<details>
<summary>24. L'API écoute sur localhost:8000 dans le conteneur. Pourquoi curl localhost:8000 échoue-t-il depuis l'hôte ?</summary>

Chaque namespace réseau a sa propre pile réseau, donc son propre `localhost`
et ses propres ports. L'API écoute sur celui du conteneur, `curl` contacte
celui de l'hôte.
</details>

<details>
<summary>25. Pourquoi Docker a-t-il besoin de root pour le réseau, et comment pasta s'en passe-t-il ?</summary>

Docker crée des interfaces (veth, pont) et des règles de pare-feu sur l'hôte,
ce qui demande root. pasta est un programme ordinaire qui relaie le trafic du
conteneur à travers des sockets normales de l'hôte, et écoute lui-même sur
les ports publiés.
</details>

<details>
<summary>26. À quoi sert le tube de synchronisation entre run et child ?</summary>

Le réseau se branche de l'extérieur, sur un conteneur qui existe déjà (pasta a
besoin de son PID). L'enfant attend donc, bloqué sur le tube, que le parent
ait fini, avant de lancer la commande.
</details>

<details>
<summary>27. Pourquoi le DNS de l'hôte (127.0.0.53) ne marche-t-il pas dans le conteneur ?</summary>

C'est une adresse de `localhost`, donc de la pile réseau de l'hôte. Dans le
conteneur, `127.0.0.53` désigne son propre `lo`, où aucun résolveur n'écoute.
pasta relaie une autre adresse (`169.254.1.1`) vers le vrai résolveur.
</details>
