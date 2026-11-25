// minibox : un mini-runtime de conteneurs, pour comprendre ce que fait Docker.
//
// Usage : minibox run <commande> [arguments...]
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage : minibox run <commande> [arguments...]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		run(os.Args[2:])
	case "child":
		child(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue : %s\n", os.Args[1])
		os.Exit(2)
	}
}

// run s'exécute sur l'hôte. Il relance minibox lui-même (/proc/self/exe) en
// mode "child", dans de nouveaux namespaces : c'est le noyau qui les crée,
// au moment du clone() du processus enfant.
func run(args []string) {
	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, args...)...)
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
func child(args []string) {
	if err := syscall.Sethostname([]byte("minibox")); err != nil {
		fail("sethostname", err)
	}

	// Les montages faits ici ne doivent pas se propager à l'hôte.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail("montages privés", err)
	}
	// ps lit /proc : on y monte un procfs neuf, qui ne montre que les
	// processus de notre namespace PID. Celui de l'hôte n'est pas touché.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fail("montage de /proc", err)
	}

	path, err := exec.LookPath(args[0])
	if err != nil {
		fail("recherche de la commande", err)
	}
	// exec() remplace le processus courant : la commande prend la place de
	// minibox, avec le même PID et les mêmes namespaces.
	if err := syscall.Exec(path, args, os.Environ()); err != nil {
		fail("exec", err)
	}
}

func fail(step string, err error) {
	fmt.Fprintf(os.Stderr, "minibox : %s : %v\n", step, err)
	os.Exit(1)
}
