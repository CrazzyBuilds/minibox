#!/bin/sh
# Extrait une image Docker couche par couche, sans les fusionner.
# Usage : ./pull-image.sh <image> <dossier>
#
# Résultat :
#   <dossier>/config.json      la configuration (Cmd, WorkingDir, Env…)
#   <dossier>/layers/00 … NN   une couche par dossier, 00 = la plus basse
set -eu

IMAGE=${1:?usage : $0 <image> <dossier>}
DEST=${2:?usage : $0 <image> <dossier>}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# docker save produit une archive au format OCI : un manifeste qui liste les
# couches dans l'ordre, une configuration, et une archive tar par couche.
docker save "$IMAGE" | tar -x -C "$TMP"
MANIFEST="$TMP/manifest.json"

rm -rf "$DEST" && mkdir -p "$DEST/layers"
cp "$TMP/$(jq -r '.[0].Config' "$MANIFEST")" "$DEST/config.json"

i=0
for layer in $(jq -r '.[0].Layers[]' "$MANIFEST"); do
	dir="$DEST/layers/$(printf '%02d' "$i")"
	mkdir -p "$dir"
	# Un fichier supprimé par une couche est noté ".wh.<nom>" (whiteout).
	# Non géré ici : il faudrait le convertir au format d'overlayfs.
	if tar -tzf "$TMP/$layer" | grep -q '\.wh\.'; then
		echo "attention : la couche $i supprime des fichiers (non géré)" >&2
	fi
	tar -xzf "$TMP/$layer" -C "$dir" --no-same-owner --exclude='dev/*'
	echo "couche $(printf '%02d' "$i") : $(du -sh "$dir" | cut -f1)	$(find "$dir" -mindepth 1 | wc -l) fichiers"
	i=$((i + 1))
done
