// minibox : un mini-runtime de conteneurs, pour comprendre ce que fait Docker.
//
// Usage : minibox run [-rootfs DIR] [-workdir DIR] <commande> [arguments...]
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Environnement de la commande : celui de l'image, pas celui de l'hôte.
var containerEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"TERM=xterm",
}

// Périphériques indispensables, empruntés à l'hôte : sans être vraiment root,
// on ne peut pas en créer (mknod), mais on peut monter ceux qui existent.
var devices = []string{"null", "zero", "random", "urandom", "tty"}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "run":
		flags := flag.NewFlagSet("run", flag.ExitOnError)
		rootfs := flags.String("rootfs", "", "dossier à utiliser comme racine (vide : celle de l'hôte)")
		workdir := flags.String("workdir", "/", "dossier courant dans le conteneur")
		_ = flags.Parse(os.Args[2:])
		if flags.NArg() == 0 {
			usage()
		}
		run(*rootfs, *workdir, flags.Args())
	case "child":
		// Appelé uniquement par run : child <rootfs> <workdir> <commande...>
		child(os.Args[2], os.Args[3], os.Args[4:])
	default:
		usage()
	}
}

// run s'exécute sur l'hôte. Il relance minibox lui-même (/proc/self/exe) en
// mode "child", dans de nouveaux namespaces : c'est le noyau qui les crée,
// au moment du clone() du processus enfant.
func run(rootfs, workdir string, args []string) {
	if rootfs != "" {
		abs, err := filepath.Abs(rootfs)
		if err != nil {
			fail("chemin du rootfs", err)
		}
		rootfs = abs
	}

	childArgs := append([]string{"child", rootfs, workdir}, args...)
	cmd := exec.Command("/proc/self/exe", childArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

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

// child s'exécute déjà dans les nouveaux namespaces. Il prépare
// l'environnement du conteneur, puis se remplace par la commande demandée.
func child(rootfs, workdir string, args []string) {
	must("sethostname", syscall.Sethostname([]byte("minibox")))

	// Les montages faits ici ne doivent pas se propager à l'hôte.
	must("montages privés", syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""))

	if rootfs != "" {
		changeRoot(rootfs)
	} else {
		// Sans rootfs, on garde les fichiers de l'hôte, avec un /proc neuf.
		mountProc("/proc")
	}

	must("chdir", os.Chdir(workdir))
	must("environnement", setEnv(containerEnv))

	path, err := exec.LookPath(args[0])
	if err != nil {
		fail("recherche de la commande", err)
	}
	// exec() remplace le processus courant : la commande prend la place de
	// minibox, avec le même PID et les mêmes namespaces.
	must("exec", syscall.Exec(path, args, containerEnv))
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

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func setEnv(env []string) error {
	os.Clearenv()
	for _, kv := range env {
		for i := range kv {
			if kv[i] == '=' {
				if err := os.Setenv(kv[:i], kv[i+1:]); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
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
	fmt.Fprintln(os.Stderr, "usage : minibox run [-rootfs DIR] [-workdir DIR] <commande> [arguments...]")
	os.Exit(2)
}
