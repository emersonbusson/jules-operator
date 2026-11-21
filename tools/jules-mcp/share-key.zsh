#!/usr/bin/env zsh

set -eu

if [[ -z "${JULES_API_KEY:-}" ]]; then
  print -u2 "Chave ausente. Execute primeiro: source tools/jules-mcp/load-key.zsh"
  exit 2
fi

runtime_root="/run/user/${EUID}"
runtime_dir="${runtime_root}/ramshared"
runtime_key_file="${runtime_dir}/jules-api-key"
runtime_temp_file=""

if [[ ! -d "$runtime_root" || ! -O "$runtime_root" || -L "$runtime_root" ]]; then
  print -u2 "Runtime do usuário ausente ou inseguro: $runtime_root"
  exit 2
fi

umask 077
command mkdir -p -m 700 "$runtime_dir"
if [[ ! -d "$runtime_dir" || ! -O "$runtime_dir" || -L "$runtime_dir" ]]; then
  print -u2 "Diretório de runtime Jules inválido."
  exit 2
fi
command chmod 700 "$runtime_dir"
runtime_temp_file="$(command mktemp "${runtime_dir}/.jules-api-key.XXXXXX")"
trap '[[ -n "${runtime_temp_file:-}" ]] && command rm -f -- "$runtime_temp_file"' EXIT HUP INT TERM
print -rn -- "$JULES_API_KEY" >"$runtime_temp_file"
command chmod 600 "$runtime_temp_file"
command mv -f -- "$runtime_temp_file" "$runtime_key_file"
runtime_temp_file=""
trap - EXIT HUP INT TERM

print "Credencial disponibilizada somente no runtime em memória."
print "Ela será removida no logout/reboot e não foi exibida."

unset runtime_root runtime_dir runtime_key_file runtime_temp_file
