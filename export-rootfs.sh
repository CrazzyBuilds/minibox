#!/bin/sh
# Extrait le système de fichiers d'une image Docker dans un dossier.
# Usage : ./export-rootfs.sh <image> <dossier>
set -eu

IMAGE=${1:?usage : $0 <image> <dossier>}
DEST=${2:?usage : $0 <image> <dossier>}

# docker export travaille sur un conteneur : on en crée un sans le démarrer
ID=$(docker create "$IMAGE")
trap 'docker rm "$ID" >/dev/null' EXIT

rm -rf "$DEST" && mkdir -p "$DEST"
# Tous les fichiers nous appartiendront (on n'est pas root) : dans le
# conteneur, notre utilisateur devient root grâce au user namespace.
# dev/ est exclu : on ne peut pas créer de périphériques sans être root.
docker export "$ID" | tar -x -C "$DEST" --no-same-owner --exclude='dev/*'

echo "$IMAGE extrait dans $DEST ($(du -sh "$DEST" | cut -f1))"
