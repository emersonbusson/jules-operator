#!/usr/bin/env zsh

set -eu

script_dir="${0:A:h}"
source "${script_dir}/runtime-key.zsh"

load_jules_runtime_key || {
  print -u2 "Jules indisponível: credencial ausente do ambiente em memória."
  exit 2
}

cd "$script_dir"
GOWORK=off exec go run .
