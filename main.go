// minibox : un mini-runtime de conteneurs, pour comprendre ce que fait Docker.
//
// Usage :
//
//	minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [commande [arguments...]]
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Environnement par défaut, quand on n'utilise pas d'image.
var defaultEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// Périphériques indispensables, empruntés à l'hôte : sans être vraiment root,
// on ne peut pas en créer (mknod), mais on peut monter ceux qui existent.
var devices = []string{"null", "zero", "random", "urandom", "tty"}

// imageConfig : la partie de config.json qui nous intéresse.
type imageConfig struct {
	Config struct {
		Cmd        []string
		Env        []string
		WorkingDir string
	} `json:"config"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "run":
		flags := flag.NewFlagSet("run", flag.ExitOnError)
		image := flags.String("image", "", "image extraite par pull-image.sh (couches + config)")
		rootfs := flags.String("rootfs", "", "dossier à utiliser tel quel comme racine")
		workdir := flags.String("workdir", "", "dossier courant dans le conteneur")
		_ = flags.Parse(os.Args[2:])
		run(*image, *rootfs, *workdir, flags.Args())
	case "child":
		// Appelé uniquement par run : child <racine> <workdir> <overlay> <commande...>
		child(os.Args[2], os.Args[3], os.Args[4], os.Args[5:])
	default:
		usage()
	}
}

// run s'exécute sur l'hôte. Il prépare ce qui peut l'être de l'extérieur, puis
// relance minibox lui-même (/proc/self/exe) en mode "child", dans de nouveaux
// namespaces : c'est le noyau qui les crée, au moment du clone() de l'enfant.
func run(image, rootfs, workdir string, args []string) {
	env := defaultEnv
	overlay := "" // options de montage overlayfs, si on utilise une image

	if image != "" {
		cfg := loadConfig(image)
		// Comme docker run : sans commande, on lance le CMD de l'image.
		if len(args) == 0 {
			args = cfg.Config.Cmd
		}
		if workdir == "" {
			workdir = cfg.Config.WorkingDir
		}
		env = cfg.Config.Env
		rootfs, overlay = prepareOverlay(image)
	}

	if len(args) == 0 {
		usage()
	}
	if workdir == "" {
		workdir = "/"
	}
	if rootfs != "" {
		rootfs = absolute(rootfs)
	}

	childArgs := append([]string{"child", rootfs, workdir, overlay}, args...)
	cmd := exec.Command("/proc/self/exe", childArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(env, "HOME=/root", "TERM=xterm")

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | // ses propres utilisateurs
			syscall.CLONE_NEWUTS | // son propre hostname
			syscall.CLONE_NEWPID | // sa propre numérotation des processus
			syscall.CLONE_NEWNS, // ses propres points de montage

		// User namespace : notre utilisateur (sur l'hôte) devient root (0)
		// dans le conteneur. Root ici, simple utilisateur pour le noyau.
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fail("lancement du conteneur", err)
	}
}

// prepareOverlay crée les dossiers propres à ce conteneur et renvoie le
// dossier où sera montée la vue fusionnée, ainsi que les options d'overlayfs.
//
//	merged  ← ce que voit le conteneur : la superposition de tout le reste
//	upper   ← les écritures de CE conteneur (lecture-écriture)
//	layers/06 … layers/00  ← les couches de l'image (lecture seule, partagées)
func prepareOverlay(image string) (merged, options string) {
	layers, err := filepath.Glob(filepath.Join(absolute(image), "layers", "*"))
	if err != nil || len(layers) == 0 {
		fail("lecture des couches", fmt.Errorf("aucune couche dans %s/layers", image))
	}
	// overlayfs attend la couche du dessus en premier : on inverse l'ordre.
	for i, j := 0, len(layers)-1; i < j; i, j = i+1, j-1 {
		layers[i], layers[j] = layers[j], layers[i]
	}

	id := newID()
	dir := absolute(filepath.Join("containers", id))
	for _, sub := range []string{"upper", "work", "merged"} {
		must("création de "+sub, os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}
	fmt.Fprintf(os.Stderr, "minibox : conteneur %s (containers/%s)\n", id, id)

	options = fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,userxattr",
		strings.Join(layers, ":"), filepath.Join(dir, "upper"), filepath.Join(dir, "work"))
	return filepath.Join(dir, "merged"), options
}

// child s'exécute déjà dans les nouveaux namespaces. Il prépare
// l'environnement du conteneur, puis se remplace par la commande demandée.
func child(rootfs, workdir, overlay string, args []string) {
	must("sethostname", syscall.Sethostname([]byte("minibox")))

	// Les montages faits ici ne doivent pas se propager à l'hôte.
	must("montages privés", syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""))

	if overlay != "" {
		// Le montage overlay se fait ici, dans notre namespace mount : l'hôte
		// ne le voit pas, et il disparaît avec le conteneur.
		must("montage overlay", syscall.Mount("overlay", rootfs, "overlay", 0, overlay))
	}

	if rootfs != "" {
		changeRoot(rootfs)
	} else {
		// Sans rootfs, on garde les fichiers de l'hôte, avec un /proc neuf.
		mountProc("/proc")
	}

	must("chdir", os.Chdir(workdir))

	path, err := exec.LookPath(args[0])
	if err != nil {
		fail("recherche de la commande", err)
	}
	// exec() remplace le processus courant : la commande prend la place de
	// minibox, avec le même PID et les mêmes namespaces.
	must("exec", syscall.Exec(path, args, os.Environ()))
}

// changeRoot fait du dossier rootfs la racine "/" du conteneur.
func changeRoot(rootfs string) {
	// pivot_root exige que la nouvelle racine soit un point de montage :
	// on monte le dossier sur lui-même.
	must("bind du rootfs", syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""))

	// pivot_root échange les racines : rootfs devient "/", et l'ancienne
	// racine (celle de l'hôte) est déplacée dans /.oldroot.
	oldroot := filepath.Join(rootfs, ".oldroot")
	must("création de .oldroot", os.MkdirAll(oldroot, 0o700))
	must("pivot_root", syscall.PivotRoot(rootfs, oldroot))
	must("chdir /", os.Chdir("/"))

	// Tant que l'hôte est encore accessible dans /.oldroot, on lui emprunte
	// ce dont on a besoin : /proc, puis quelques périphériques.
	mountProc("/proc")
	for _, dev := range devices {
		target := "/dev/" + dev
		must("création de "+target, touch(target))
		must("montage de "+target, syscall.Mount("/.oldroot"+target, target, "", syscall.MS_BIND, ""))
	}

	// On détache l'ancienne racine : plus aucun fichier de l'hôte n'est visible.
	must("démontage de l'hôte", syscall.Unmount("/.oldroot", syscall.MNT_DETACH))
	must("suppression de /.oldroot", os.Remove("/.oldroot"))
}

// mountProc monte un procfs neuf : il ne montre que les processus de notre
// namespace PID. Celui de l'hôte n'est pas touché.
func mountProc(target string) {
	must("création de "+target, os.MkdirAll(target, 0o555))
	must("montage de "+target, syscall.Mount("proc", target, "proc", 0, ""))
}

func loadConfig(image string) imageConfig {
	var cfg imageConfig
	data, err := os.ReadFile(filepath.Join(image, "config.json"))
	if err == nil {
		err = json.Unmarshal(data, &cfg)
	}
	if err != nil {
		fail("lecture de la configuration de l'image", err)
	}
	return cfg
}

func newID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func absolute(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		fail("chemin "+path, err)
	}
	return abs
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func must(step string, err error) {
	if err != nil {
		fail(step, err)
	}
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "minibox : %s : %v\n", step, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage : minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [commande [arguments...]]")
	os.Exit(2)
}
