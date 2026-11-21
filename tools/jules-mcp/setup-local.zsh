#!/usr/bin/env zsh

set -eu
umask 077

script_dir="${0:A:h}"
repo_root="${script_dir:h:h}"
test_mode="${JULES_LOCAL_TEST_MODE:-false}"
ramshared_repo="${RAMSHARED_REPO:-}"
rotate=false

usage() {
  print "Uso: tools/jules-mcp/setup-local.zsh [--repo /caminho/ramshared] [--rotate]"
}

while (( $# > 0 )); do
  case "$1" in
    --repo)
      (( $# >= 2 )) || {
        print -u2 "Valor ausente para --repo."
        exit 2
      }
      ramshared_repo="$2"
      shift 2
      ;;
    --rotate)
      rotate=true
      shift
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

credential_helper="${script_dir}/credential-store.py"
publisher="${script_dir}/share-key.zsh"
hook_installer="${script_dir}/install-local-hook.zsh"
scheduler="${script_dir}/local-scheduler.zsh"
if [[ "$test_mode" == "true" ]]; then
  credential_helper="${JULES_CREDENTIAL_HELPER:-$credential_helper}"
  publisher="${JULES_KEY_PUBLISHER:-$publisher}"
  hook_installer="${JULES_LOCAL_HOOK_INSTALLER:-$hook_installer}"
  scheduler="${JULES_LOCAL_SCHEDULER:-$scheduler}"
fi

for helper in "$credential_helper" "$publisher" "$hook_installer" "$scheduler"; do
  [[ -x "$helper" ]] || {
    print -u2 "Helper local ausente ou sem permissão: $helper"
    exit 2
  }
  if [[ "$test_mode" != "true" && ( ! -O "$helper" || -L "$helper" ) ]]; then
    print -u2 "Helper local inseguro: $helper"
    exit 2
  fi
done

config_root="${XDG_CONFIG_HOME:-${HOME}/.config}"
config_dir="${config_root}/ramshared/jules-operator"
config_file="${config_dir}/config"
if [[ -z "$ramshared_repo" && -f "$config_file" && ! -L "$config_file" ]]; then
  ramshared_repo="$(command sed -n 's/^RAMSHARED_REPO=//p' "$config_file" | command head -n 1)"
fi
if [[ -z "$ramshared_repo" && -d "${repo_root:h}/ramshared-work" ]]; then
  ramshared_repo="${repo_root:h}/ramshared-work"
fi
[[ -n "$ramshared_repo" ]] || {
  print -u2 "Checkout RamShared não encontrado; use --repo uma única vez."
  exit 2
}
ramshared_repo="${ramshared_repo:A}"

if [[ "$test_mode" != "true" ]]; then
  [[ -d "$ramshared_repo/.git" || -f "$ramshared_repo/.git" ]] || {
    print -u2 "Checkout Git inválido: $ramshared_repo"
    exit 2
  }
  remote="$(command git -C "$ramshared_repo" remote get-url origin 2>/dev/null)" || {
    print -u2 "O checkout RamShared não possui remote origin."
    exit 2
  }
  case "$remote" in
    https://github.com/emersonbusson/ramshared | https://github.com/emersonbusson/ramshared.git | \
      git@github.com:emersonbusson/ramshared | git@github.com:emersonbusson/ramshared.git | \
      ssh://git@github.com/emersonbusson/ramshared | \
      ssh://git@github.com/emersonbusson/ramshared.git)
      ;;
    *)
      print -u2 "Remote recusado; esperado emersonbusson/ramshared."
      exit 2
      ;;
  esac
  unset remote
fi

command mkdir -p -m 700 -- "$config_dir"
[[ -d "$config_dir" && -O "$config_dir" && ! -L "$config_dir" ]] || {
  print -u2 "Diretório de configuração Jules inseguro."
  exit 2
}
command chmod 700 -- "$config_dir"
config_temp="$(command mktemp "${config_dir}/.config.XXXXXX")"
trap '[[ -z "${config_temp:-}" ]] || command unlink "$config_temp" 2>/dev/null || true' \
  EXIT HUP INT TERM
print -r -- "RAMSHARED_REPO=$ramshared_repo" >"$config_temp"
command chmod 600 -- "$config_temp"
command mv -f -- "$config_temp" "$config_file"
config_temp=""
trap - EXIT HUP INT TERM

source "${script_dir}/runtime-key.zsh"
source "${script_dir}/validate-key.zsh"

key_validated=false
if [[ "$rotate" == "true" ]]; then
  unset JULES_API_KEY
  source "${script_dir}/load-key.zsh"
  key_validated=true
elif ! load_jules_runtime_key; then
  source "${script_dir}/load-key.zsh"
  key_validated=true
fi

if [[ "$test_mode" != "true" && "$key_validated" != "true" ]]; then
  validate_jules_key
fi

print -rn -- "$JULES_API_KEY" |
  command env -u JULES_API_KEY "$credential_helper" write
"$publisher"
unset JULES_API_KEY
"$hook_installer" install
"$scheduler" start --repo "$ramshared_repo"

unset helper key_validated credential_helper publisher
unset hook_installer scheduler ramshared_repo config_root config_dir config_file
print "Configuração Jules concluída."
print "A chave ficou no chaveiro de login e será reutilizada após reboot."
print "Para trocar uma chave exposta, execute este mesmo setup com --rotate."
