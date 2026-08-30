# Environnement de dev : Go pour `make check` / `make build`.
# Utilisation : nix-shell --run 'make check'
{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = [
    pkgs.go
    pkgs.gnumake
  ];
}
