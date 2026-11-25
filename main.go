// minibox : un mini-runtime de conteneurs, pour comprendre ce que fait Docker.
//
// Usage :
//
//	minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [-memory 256M] [-cpus 0.5] [-pids 64] [-fake-meminfo] [-net none|host|pasta] [-p 8000:8000] [commande [arguments...]]
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
	"unsafe"
)

// Environnement par défaut, quand on n'utilise pas d'image.
var defaultEnv = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

// Périphériques indispensables, empruntés à l'hôte : sans être vraiment root,
// on ne peut pas en créer (mknod), mais on peut monter ceux qui existent.
var devices = []string{"null", "zero", "random", "urandom", "tty"}

// childConfig : ce que run transmet à child pour préparer le conteneur.
type childConfig struct {
	Rootfs      string // racine du conteneur (vide : celle de l'hôte)
	Workdir     string
	Overlay     string // options de montage overlayfs (vide : pas d'overlay)
	FakeMeminfo bool
	Net         string // "none", "host" ou "pasta"
}

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
		fakeMeminfo := flags.Bool("fake-meminfo", false, "/proc/meminfo affiche la limite mémoire (comme lxcfs)")
		netMode := flags.String("net", "none", "réseau : none (isolé), host (celui de l'hôte) ou pasta")
		var ports portList
		flags.Var(&ports, "p", "publie un port, ex. -p 8000:8000 (hôte:conteneur), implique -net pasta")
		_ = flags.Parse(os.Args[2:])
		if len(ports) > 0 {
			*netMode = "pasta"
		}
		cfg := childConfig{Rootfs: *rootfs, Workdir: *workdir, FakeMeminfo: *fakeMeminfo, Net: *netMode}
		run(*image, cfg, lim, ports, flags.Args())
	case "child":
		// Appelé uniquement par run : child <configuration JSON> <commande...>
		var cfg childConfig
		must("configuration de l'enfant", json.Unmarshal([]byte(os.Args[2]), &cfg))
		child(cfg, os.Args[3:])
	default:
		usage()
	}
}

// run s'exécute sur l'hôte. Il prépare ce qui peut l'être de l'extérieur, puis
// relance minibox lui-même (/proc/self/exe) en mode "child", dans de nouveaux
// namespaces : c'est le noyau qui les crée, au moment du clone() de l'enfant.
func run(image string, cfg childConfig, lim limits, ports portList, args []string) {
	env := defaultEnv
	id := newID()

	if image != "" {
		img := loadConfig(image)
		// Comme docker run : sans commande, on lance le CMD de l'image.
		if len(args) == 0 {
			args = img.Config.Cmd
		}
		if cfg.Workdir == "" {
			cfg.Workdir = img.Config.WorkingDir
		}
		env = img.Config.Env
		cfg.Rootfs, cfg.Overlay = prepareOverlay(image, id)
	}

	if len(args) == 0 {
		usage()
	}
	if cfg.Workdir == "" {
		cfg.Workdir = "/"
	}
	if cfg.Rootfs != "" {
		cfg.Rootfs = absolute(cfg.Rootfs)
	}
	if cfg.Net != "none" && cfg.Net != "host" && cfg.Net != "pasta" {
		fail("-net", fmt.Errorf("mode inconnu : %q", cfg.Net))
	}

	cfgJSON, _ := json.Marshal(cfg)
	cmd := exec.Command("/proc/self/exe", append([]string{"child", string(cfgJSON)}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(env, "HOME=/root", "TERM=xterm")

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | // ses propres utilisateurs
			syscall.CLONE_NEWUTS | // son propre hostname
			syscall.CLONE_NEWPID | // sa propre numérotation des processus
			syscall.CLONE_NEWNS | // ses propres points de montage
			syscall.CLONE_NEWCGROUP, // son cgroup lui apparaît comme la racine

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

	// Réseau isolé, sauf -net host.
	if cfg.Net != "host" {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWNET
	}

	// Tube de synchronisation : l'enfant attend notre feu vert avant de
	// lancer la commande, le temps qu'on branche son réseau de l'extérieur.
	syncR, syncW, err := os.Pipe()
	must("tube de synchronisation", err)
	cmd.ExtraFiles = []*os.File{syncR} // devient le descripteur 3 de l'enfant

	must("démarrage du conteneur", cmd.Start())
	_ = syncR.Close()
	if cfg.Net == "pasta" {
		if err := startPasta(cmd.Process.Pid, ports); err != nil {
			_ = cmd.Process.Kill()
			fail("pasta", err)
		}
	}
	_ = syncW.Close() // feu vert : l'enfant lit "fin de fichier" et continue

	err = cmd.Wait()
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
func child(cfg childConfig, args []string) {
	rootfs, workdir := cfg.Rootfs, cfg.Workdir

	// Attend le feu vert du parent (fermeture du tube, descripteur 3).
	waitFile := os.NewFile(3, "sync")
	_, _ = waitFile.Read(make([]byte, 1))
	_ = waitFile.Close()

	if cfg.Net != "host" {
		// Un namespace réseau neuf ne contient que "lo", éteinte.
		must("activation de lo", loopbackUp())
	}

	must("sethostname", syscall.Sethostname([]byte("minibox")))

	// Les montages faits ici ne doivent pas se propager à l'hôte.
	must("montages privés", syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""))

	if cfg.Overlay != "" {
		// Le montage overlay se fait ici, dans notre namespace mount : l'hôte
		// ne le voit pas, et il disparaît avec le conteneur.
		must("montage overlay", syscall.Mount("overlay", rootfs, "overlay", 0, cfg.Overlay))
	}

	if rootfs != "" {
		changeRoot(rootfs)
		if cfg.FakeMeminfo {
			mountFakeMeminfo()
		}
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
	mountCgroup("/sys/fs/cgroup")
	for _, dev := range devices {
		target := "/dev/" + dev
		must("création de "+target, touch(target))
		must("montage de "+target, syscall.Mount("/.oldroot"+target, target, "", syscall.MS_BIND, ""))
	}

	// On détache l'ancienne racine : plus aucun fichier de l'hôte n'est visible.
	must("démontage de l'hôte", syscall.Unmount("/.oldroot", syscall.MNT_DETACH))
	must("suppression de /.oldroot", os.Remove("/.oldroot"))
}

// loopbackUp allume l'interface "lo" du namespace réseau (l'équivalent de
// "ip link set lo up"), avec un ioctl : on modifie les drapeaux de l'interface.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr struct { // struct ifreq du noyau
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], "lo")
	ifr.flags = syscall.IFF_UP | syscall.IFF_LOOPBACK | syscall.IFF_RUNNING
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		return errno
	}
	return nil
}

// startPasta branche le namespace réseau du conteneur à l'extérieur, sans
// être root. pasta (projet passt, utilisé par Podman) rejoint le namespace,
// y crée une interface, et relaie le trafic à travers des sockets ordinaires
// de l'hôte. Il se met en arrière-plan une fois prêt, et s'arrête tout seul
// quand le namespace disparaît.
func startPasta(pid int, ports portList) error {
	args := []string{"--config-net", "--quiet"}
	for _, p := range ports {
		args = append(args, "-t", p) // TCP : port de l'hôte → port du conteneur
	}
	if len(ports) == 0 {
		args = append(args, "-t", "none")
	}
	args = append(args, "-u", "none", fmt.Sprint(pid))

	out, err := exec.Command("pasta", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, out)
	}
	return nil
}

// portList : les options -p, qui peuvent être répétées.
type portList []string

func (p *portList) String() string     { return strings.Join(*p, ",") }
func (p *portList) Set(v string) error { *p = append(*p, v); return nil }

// mountCgroup monte le système de fichiers des cgroups. Grâce au namespace
// cgroup, sa racine est le cgroup du conteneur : le programme y lit ses
// propres limites (memory.max, cpu.max…), et rien de ce qui est au-dessus.
func mountCgroup(target string) {
	must("création de "+target, os.MkdirAll(target, 0o555))
	must("montage de "+target, syscall.Mount("cgroup2", target, "cgroup2",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""))
}

// mountFakeMeminfo recouvre /proc/meminfo par un fichier qui annonce la
// limite du cgroup au lieu de la RAM de la machine, comme le fait lxcfs.
// Différence : lxcfs régénère le contenu à chaque lecture (avec FUSE), ici
// c'est une photo prise au démarrage, qui ne bougera plus.
func mountFakeMeminfo() {
	max := readCgroupValue("/sys/fs/cgroup/memory.max")
	if max <= 0 {
		return // pas de limite mémoire : le vrai /proc/meminfo est juste
	}
	free := max - readCgroupValue("/sys/fs/cgroup/memory.current")
	content := fmt.Sprintf("MemTotal: %d kB\nMemFree: %d kB\nMemAvailable: %d kB\n"+
		"Buffers: 0 kB\nCached: 0 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n", max/1024, free/1024, free/1024)

	const fake = "/run/minibox-meminfo"
	must("création de "+fake, writeFile(fake, content))
	must("montage de /proc/meminfo", syscall.Mount(fake, "/proc/meminfo", "", syscall.MS_BIND, ""))
}

// readCgroupValue lit un nombre dans un fichier de cgroup ("max" = -1).
func readCgroupValue(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		fail("lecture de "+path, err)
	}
	var v int64
	if _, err := fmt.Sscanf(string(data), "%d", &v); err != nil {
		return -1
	}
	return v
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
	fmt.Fprintln(os.Stderr, "usage : minibox run [-image DIR | -rootfs DIR] [-workdir DIR] [-memory 256M] [-cpus 0.5] [-pids 64] [-fake-meminfo] [-net none|host|pasta] [-p 8000:8000] [commande [arguments...]]")
	os.Exit(2)
}
