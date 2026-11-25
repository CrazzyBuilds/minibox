// minibox : un mini-runtime de conteneurs, pour comprendre ce que fait Docker.
//
// Usage :
//
//	minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [-memory 256M] [-cpus 0.5] [-pids 64] [commande [arguments...]]
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
		var lim limits
		flags.StringVar(&lim.memory, "memory", "", "mémoire maximale (ex. 256M, 1G)")
		flags.StringVar(&lim.cpus, "cpus", "", "nombre de CPU utilisables (ex. 0.5)")
		flags.StringVar(&lim.pids, "pids", "", "nombre maximal de processus")
		_ = flags.Parse(os.Args[2:])
		run(*image, *rootfs, *workdir, lim, flags.Args())
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
func run(image, rootfs, workdir string, lim limits, args []string) {
	env := defaultEnv
	overlay := "" // options de montage overlayfs, si on utilise une image
	id := newID()

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
		rootfs, overlay = prepareOverlay(image, id)
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

	// cgroup : l'enfant naît directement dans son cgroup (CLONE_INTO_CGROUP),
	// il est donc limité dès sa première instruction.
	cgroup, fd := createCgroup(id, lim)
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = fd

	err := cmd.Run()
	_ = syscall.Close(fd)
	if n := oomKills(cgroup); n > 0 {
		fmt.Fprintf(os.Stderr, "minibox : le noyau a tué %d processus du conteneur : limite mémoire dépassée (OOM)\n", n)
	}
	_ = os.Remove(cgroup) // un cgroup sans processus se supprime avec rmdir

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				os.Exit(128 + int(ws.Signal())) // convention des shells : 128 + signal
			}
			os.Exit(exitErr.ExitCode())
		}
		fail("lancement du conteneur", err)
	}
}

// limits : les limites demandées sur la ligne de commande (vide = aucune).
type limits struct {
	memory, cpus, pids string
}

// cgroupParent : la branche de l'arbre des cgroups que systemd nous délègue.
// Elle nous appartient : on peut y créer des sous-cgroups sans être root.
func cgroupParent() string {
	uid := os.Getuid()
	return fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/minibox", uid, uid)
}

// createCgroup crée le cgroup du conteneur et y écrit ses limites. Un cgroup
// est un dossier, et chaque limite un fichier : on configure en écrivant.
func createCgroup(id string, lim limits) (path string, fd int) {
	parent := cgroupParent()
	must("création du cgroup minibox", os.MkdirAll(parent, 0o755))
	// Active les contrôleurs pour les cgroups enfants de "minibox".
	must("activation des contrôleurs", writeFile(filepath.Join(parent, "cgroup.subtree_control"), "+cpu +memory +pids"))

	path = filepath.Join(parent, id)
	must("création du cgroup "+id, os.Mkdir(path, 0o755))

	if lim.memory != "" {
		must("memory.max", writeFile(filepath.Join(path, "memory.max"), parseSize(lim.memory)))
		// Sans ça, le conteneur passerait en swap au lieu d'être limité.
		must("memory.swap.max", writeFile(filepath.Join(path, "memory.swap.max"), "0"))
	}
	if lim.cpus != "" {
		// cpu.max = "<quota> <période>" en microsecondes : 0.5 CPU =
		// 50 ms de calcul autorisées toutes les 100 ms.
		var cpus float64
		if _, err := fmt.Sscanf(lim.cpus, "%g", &cpus); err != nil || cpus <= 0 {
			fail("-cpus", fmt.Errorf("valeur invalide : %q", lim.cpus))
		}
		must("cpu.max", writeFile(filepath.Join(path, "cpu.max"), fmt.Sprintf("%d 100000", int(cpus*100000))))
	}
	if lim.pids != "" {
		must("pids.max", writeFile(filepath.Join(path, "pids.max"), lim.pids))
	}

	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		fail("ouverture du cgroup", err)
	}
	return path, fd
}

// oomKills lit dans memory.events combien de processus le noyau a tués
// faute de mémoire.
func oomKills(cgroup string) int {
	data, err := os.ReadFile(filepath.Join(cgroup, "memory.events"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "oom_kill %d", &n); err == nil {
			return n
		}
	}
	return 0
}

// parseSize convertit "256M" ou "1G" en octets ("268435456").
func parseSize(s string) string {
	units := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30}
	mult, n := int64(1), s
	if u, ok := units[s[len(s)-1]]; ok {
		mult, n = u, s[:len(s)-1]
	}
	var v int64
	if _, err := fmt.Sscanf(n, "%d", &v); err != nil || v <= 0 {
		fail("-memory", fmt.Errorf("valeur invalide : %q", s))
	}
	return fmt.Sprint(v * mult)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// prepareOverlay crée les dossiers propres à ce conteneur et renvoie le
// dossier où sera montée la vue fusionnée, ainsi que les options d'overlayfs.
//
//	merged  ← ce que voit le conteneur : la superposition de tout le reste
//	upper   ← les écritures de CE conteneur (lecture-écriture)
//	layers/06 … layers/00  ← les couches de l'image (lecture seule, partagées)
func prepareOverlay(image, id string) (merged, options string) {
	layers, err := filepath.Glob(filepath.Join(absolute(image), "layers", "*"))
	if err != nil || len(layers) == 0 {
		fail("lecture des couches", fmt.Errorf("aucune couche dans %s/layers", image))
	}
	// overlayfs attend la couche du dessus en premier : on inverse l'ordre.
	for i, j := 0, len(layers)-1; i < j; i, j = i+1, j-1 {
		layers[i], layers[j] = layers[j], layers[i]
	}

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
	fmt.Fprintln(os.Stderr, "usage : minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [-memory 256M] [-cpus 0.5] [-pids 64] [commande [arguments...]]")
	os.Exit(2)
}
