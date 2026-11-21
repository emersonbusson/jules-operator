#!/usr/bin/env zsh

# Este arquivo precisa ser carregado com `source` para exportar a chave no shell
# atual. Executá-lo como subprocesso perderia a variável ao encerrar.
if [[ "${ZSH_EVAL_CONTEXT:-}" != *:file ]]; then
  print -u2 "Use: source tools/jules-mcp/load-key.zsh"
  exit 2
fi

load_key_script_dir="${${(%):-%x}:A:h}"
source "${load_key_script_dir}/validate-key.zsh"

_read_jules_key() {
  JULES_API_KEY=""
  key_character=""

  print "Cole a chave Jules."
  print "Enter valida | Esc cancela | Backspace corrige | Ctrl-C interrompe"
  print -n "> "

  while true; do
    if ! read -rsk 1 key_character; then
      print
      unset key_character
      return 2
    fi

    case "$key_character" in
      $'\e')
        print
        unset JULES_API_KEY key_character
        return 130
        ;;
      $'\r' | $'\n')
        print
        unset key_character
        return 0
        ;;
      $'\b' | $'\177')
        if (( ${#JULES_API_KEY} > 0 )); then
          JULES_API_KEY="${JULES_API_KEY[1,-2]}"
        fi
        ;;
      *)
        JULES_API_KEY+="$key_character"
        ;;
    esac
  done
}

while true; do
  _read_jules_key
  read_status=$?
  if (( read_status != 0 )); then
    if (( read_status == 130 )); then
      print "Operação cancelada com Esc; nenhuma variável foi exportada."
    else
      print "Entrada encerrada; nenhuma variável foi exportada."
    fi
    unset -f _read_jules_key
    unset read_status
    return 2
  fi

  # A chave não contém espaços. Remove whitespace introduzido por seleção,
  # quebra de linha do clipboard ou colagem em terminal remoto.
  raw_key_length=${#JULES_API_KEY}
  JULES_API_KEY="${JULES_API_KEY//[[:space:]]/}"
  normalized_key_length=${#JULES_API_KEY}

  if [[ -z "$JULES_API_KEY" ]]; then
    print "Operação cancelada; nenhuma variável foi exportada."
    unset JULES_API_KEY
    unset raw_key_length normalized_key_length
    unset -f _read_jules_key
    return 2
  fi

  if (( normalized_key_length < raw_key_length )); then
    print "Espaços invisíveis da colagem foram removidos."
  fi

  if ! validate_jules_key; then
    print -u2 "Revise a chave e tente novamente; Enter vazio cancela."
    unset JULES_API_KEY
    continue
  fi

  break
done

export JULES_API_KEY
unset -f _read_jules_key
unset raw_key_length normalized_key_length load_key_script_dir
print "JULES_API_KEY exportada nesta sessão sem exibir o valor."
print "O setup local pode persistir a chave no chaveiro de login."
