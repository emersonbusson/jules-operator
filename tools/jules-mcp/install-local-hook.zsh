#!/usr/bin/env zsh

set -eu
umask 077

script_dir="${0:A:h}"
scheduler_path="${script_dir}/local-scheduler.zsh"
zshrc_dir="${ZDOTDIR:-${HOME}}"
zshrc_path="${zshrc_dir}/.zshrc"
begin_marker="# RAMSHARED-JULES-LOCAL:BEGIN"
end_marker="# RAMSHARED-JULES-LOCAL:END"

usage() {
  print "Uso: tools/jules-mcp/install-local-hook.zsh install|status|uninstall"
}

strip_block() {
  local source_path="$1"
  local output_path="$2"
  command awk -v begin="$begin_marker" -v end="$end_marker" '
    $0 == begin { skipping = 1; next }
    $0 == end { skipping = 0; next }
    !skipping { print }
  ' "$source_path" >"$output_path"
}

install_hook() {
  [[ -x "$scheduler_path" ]] || {
    print -u2 "Scheduler local ausente ou sem permissão."
    return 2
  }
  command mkdir -p -- "$zshrc_dir"
  [[ -f "$zshrc_path" ]] || : >"$zshrc_path"
  local temporary
  temporary="$(command mktemp "${zshrc_dir}/.zshrc.ramshared.XXXXXX")"
  trap 'command rm -f -- "${temporary:-}"' EXIT HUP INT TERM
  strip_block "$zshrc_path" "$temporary"
  print -r -- "$begin_marker" >>"$temporary"
  print -r -- "_ramshared_jules_local_on_entry() {" >>"$temporary"
  print -r -- "  local repo remote" >>"$temporary"
  print -r -- \
    '  repo="$(command git -C "$PWD" rev-parse --show-toplevel 2>/dev/null)" || return 0' \
    >>"$temporary"
  print -r -- \
    '  remote="$(command git -C "$repo" remote get-url origin 2>/dev/null)" || return 0' \
    >>"$temporary"
  print -r -- '  case "$remote" in' >>"$temporary"
  print -r -- \
    '    https://github.com/emersonbusson/ramshared|https://github.com/emersonbusson/ramshared.git|git@github.com:emersonbusson/ramshared|git@github.com:emersonbusson/ramshared.git|ssh://git@github.com/emersonbusson/ramshared|ssh://git@github.com/emersonbusson/ramshared.git) ;;' \
    >>"$temporary"
  print -r -- '    *) return 0 ;;' >>"$temporary"
  print -r -- '  esac' >>"$temporary"
  print -r -- \
    "  ${(q)scheduler_path} start --repo \"\$repo\" --quiet >/dev/null 2>&1" \
    >>"$temporary"
  print -r -- "}" >>"$temporary"
  print -r -- "autoload -Uz add-zsh-hook" >>"$temporary"
  print -r -- \
    "add-zsh-hook -d chpwd _ramshared_jules_local_on_entry 2>/dev/null || true" \
    >>"$temporary"
  print -r -- \
    "add-zsh-hook chpwd _ramshared_jules_local_on_entry" >>"$temporary"
  print -r -- "_ramshared_jules_local_on_entry" >>"$temporary"
  print -r -- "$end_marker" >>"$temporary"
  command chmod --reference="$zshrc_path" "$temporary" 2>/dev/null ||
    command chmod 600 "$temporary"
  command mv -f -- "$temporary" "$zshrc_path"
  trap - EXIT HUP INT TERM
  print "Hook local instalado em $zshrc_path."
  print "Abra outro shell ou execute: source \"$zshrc_path\""
}

uninstall_hook() {
  [[ -f "$zshrc_path" ]] || {
    print "Hook local já estava ausente."
    return 0
  }
  local temporary
  temporary="$(command mktemp "${zshrc_dir}/.zshrc.ramshared.XXXXXX")"
  trap 'command rm -f -- "${temporary:-}"' EXIT HUP INT TERM
  strip_block "$zshrc_path" "$temporary"
  command chmod --reference="$zshrc_path" "$temporary" 2>/dev/null ||
    command chmod 600 "$temporary"
  command mv -f -- "$temporary" "$zshrc_path"
  trap - EXIT HUP INT TERM
  print "Hook local removido de $zshrc_path."
}

case "${1:-}" in
  install)
    install_hook
    ;;
  status)
    if [[ -f "$zshrc_path" ]] &&
      command grep -Fq -- "$begin_marker" "$zshrc_path"; then
      print "Hook local instalado em $zshrc_path."
      exit 0
    fi
    print "Hook local não instalado."
    exit 1
    ;;
  uninstall)
    uninstall_hook
    ;;
  *)
    usage
    exit 2
    ;;
esac
