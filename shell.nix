# Development environment: Go for `make check` / `make build`.
# Usage: nix-shell --run 'make check'
{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = [
    pkgs.go
    pkgs.gnumake
  ];
}
